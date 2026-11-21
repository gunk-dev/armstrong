package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// platforms maps each supported Nix system to the only image platform it
// accepts.
var platforms = map[string]struct{ os, arch string }{
	"x86_64-linux":  {"linux", "amd64"},
	"aarch64-linux": {"linux", "arm64"},
}

func packagedSystem() (string, error) {
	sys := nixSystem
	if sys == "" {
		switch runtime.GOARCH {
		case "amd64":
			sys = "x86_64-linux"
		case "arm64":
			sys = "aarch64-linux"
		}
	}
	if _, ok := platforms[sys]; !ok {
		return "", fmt.Errorf("unsupported Nix system %q", sys)
	}
	return sys, nil
}

// result is how a phase ended.
type result struct {
	outcome string
	reason  string
}

func failed(format string, args ...any) result {
	return result{outcome: outcomeFailed, reason: fmt.Sprintf(format, args...)}
}

func completed() result { return result{outcome: outcomeCompleted} }

// step is one phase of an attempt. A step with skip set never runs.
type step struct {
	name string
	skip string
	run  func(context.Context) result
}

type secrets struct {
	flyToken      string
	porkbunKey    string
	porkbunSecret string
}

// session holds the state acquired once per deploy invocation, under one lock.
type session struct {
	system         string
	verifyTimeout  time.Duration
	verifyInterval time.Duration
	tools          map[string]string
	secrets        secrets
	stateDir       string
	lock           *os.File
	gate           *startGate
	stdout         io.Writer
	stderr         io.Writer
}

// attempt is one deployment attempt from record creation to final record.
type attempt struct {
	req     request
	system  string
	secrets secrets
	r       *runner
	rec     *record
	runDir  string
	work    string // removed by cleanup: checkout, build links, credentials, temporaries
	repo    string // work/src, the detached checkout
	stdout  io.Writer
	stderr  io.Writer

	verifyTimeout  time.Duration
	verifyInterval time.Duration

	// checkSkip enables converge's skip rule during inspect for an app attempt.
	checkSkip bool
	// Set by inspectApp when checkSkip is true.
	skipped          bool   // app is already at this commit's outputs
	skipAfterInspect string // reason for skipping upload, apply and verify
	deployReason     string // why an app would deploy (dry-run summary)

	// Learned during the attempt.
	app        string // Fly app named by fly.json
	configFile string // fly.json in the store
	// requireStarted: fly.json keeps a Machine running, so verification
	// requires one started.
	requireStarted bool
	imagePath      string   // the OCI archive in the store
	digest         string   // the image manifest digest
	zonesFile      string   // aggregated DNS zones
	auth           *os.File // the unlinked registry auth file, closed by cleanup

	recordFailed bool
	red          *redactor
}

// execute runs a validated request and returns the exit status.
func execute(req request, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	// A reader that goes away would otherwise kill deploy with SIGPIPE at its
	// next line of output, mid-attempt; caught, the write just fails. Unlike
	// ignoring it, catching it leaves children the default disposition.
	pipes := make(chan os.Signal, 1)
	signal.Notify(pipes, syscall.SIGPIPE)
	defer signal.Stop(pipes)
	var got atomic.Int32
	gate := &startGate{}
	go func() {
		for s := range sigs {
			sig := s.(syscall.Signal)
			if got.CompareAndSwap(0, int32(sig)) {
				name := map[syscall.Signal]string{syscall.SIGINT: "SIGINT", syscall.SIGTERM: "SIGTERM", syscall.SIGHUP: "SIGHUP"}[sig]
				gate.close(func() { cancel(fmt.Errorf("interrupted by %s", name)) })
			}
		}
	}()

	code := executeAttempt(ctx, gate, req, stdout, stderr)
	switch syscall.Signal(got.Load()) {
	case syscall.SIGINT:
		return exitSIGINT
	case syscall.SIGTERM:
		return exitSIGTERM
	case syscall.SIGHUP:
		return exitSIGHUP
	}
	return code
}

func executeAttempt(ctx context.Context, gate *startGate, req request, stdout, stderr io.Writer) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "deploy: "+format+"\n", args...)
		return exitFailure
	}
	// Everything this process and its children create is private.
	syscall.Umask(0o077)

	system, err := packagedSystem()
	if err != nil {
		return fail("%v", err)
	}
	vTimeout, err1 := time.ParseDuration(verifyTimeout)
	vInterval, err2 := time.ParseDuration(verifyInterval)
	if err := errors.Join(err1, err2); err != nil {
		return fail("verification bounds: %v", err)
	}
	tools, err := resolveTools(req.verb)
	if err != nil {
		return fail("%v", err)
	}
	// A credential file can be a pipe nothing writes to; a signal must still
	// end the wait.
	type read struct {
		sec secrets
		err error
	}
	reads := make(chan read, 1)
	go func() {
		sec, err := readSecrets(req)
		reads <- read{sec, err}
	}()
	var sec secrets
	select {
	case r := <-reads:
		if r.err != nil {
			return fail("%v", r.err)
		}
		sec = r.sec
	case <-ctx.Done():
		return exitFailure
	}
	if ctx.Err() != nil {
		return exitFailure
	}

	// Directories created now must survive a crash along with the records in
	// them, so each new one's entry in its parent is synced.
	var created []string
	for d := req.stateDir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); !errors.Is(err, fs.ErrNotExist) || d == filepath.Dir(d) {
			break
		}
		created = append(created, d)
	}
	if err := os.MkdirAll(req.stateDir, 0o700); err != nil {
		return fail("%v", err)
	}
	for _, d := range created {
		if err := syncDir(filepath.Dir(d)); err != nil {
			return fail("%v", err)
		}
	}
	if err := checkStateDir(req.stateDir); err != nil {
		return fail("DEPLOY_STATE_DIR: %v", err)
	}
	lockPath := filepath.Join(req.stateDir, "deploy.lock")
	lock, err := acquireLock(lockPath)
	if errors.Is(err, errContended) {
		fmt.Fprintf(stderr, "deploy: another attempt holds %s\n", lockPath)
		return exitContention
	}
	if err != nil {
		return fail("lock: %v", err)
	}
	// Closing, never unlocking: a mutating child that outlived its deadline
	// still holds the inherited descriptor, and with it the lock.
	defer lock.Close()

	sess := &session{
		system:         system,
		verifyTimeout:  vTimeout,
		verifyInterval: vInterval,
		tools:          tools,
		secrets:        sec,
		stateDir:       req.stateDir,
		lock:           lock,
		gate:           gate,
		stdout:         stdout,
		stderr:         stderr,
	}
	if req.verb == verbConverge {
		return runConverge(ctx, sess, req)
	}
	a, err := runAttempt(ctx, sess, req, false)
	if err != nil {
		return fail("create record: %v", err)
	}
	if a.recordFailed || a.rec.Outcome != outcomeCompleted {
		return exitFailure
	}
	return exitOK
}

// runAttempt runs one ordinary app or dns-sync attempt under sess's lock.
func runAttempt(ctx context.Context, sess *session, req request, checkSkip bool) (*attempt, error) {
	a := &attempt{
		req:            req,
		system:         sess.system,
		secrets:        sess.secrets,
		stdout:         sess.stdout,
		stderr:         sess.stderr,
		verifyTimeout:  sess.verifyTimeout,
		verifyInterval: sess.verifyInterval,
		checkSkip:      checkSkip,
	}
	if err := a.create(); err != nil {
		return nil, err
	}
	a.r = &runner{
		tools:   sess.tools,
		path:    toolPath,
		home:    filepath.Join(a.work, "home"),
		runtime: filepath.Join(a.work, "run"),
		tmp:     filepath.Join(a.work, "tmp"),
		lock:    sess.lock,
		grace:   killGrace,
		gate:    sess.gate,
		logs:    filepath.Join(a.runDir, "logs"),
		red:     a.redactor(),
		stderr:  sess.stderr,
	}
	// An operator who cannot see where the record is gets no work done.
	if _, err := fmt.Fprintln(sess.stdout, filepath.Join(a.runDir, "record.json")); err != nil {
		var stop context.CancelCauseFunc
		ctx, stop = context.WithCancelCause(ctx)
		stop(fmt.Errorf("could not print the record path: %w", err))
	}

	a.runPhases(ctx, a.steps())
	fmt.Fprintf(sess.stderr, "deploy: attempt %s%s\n", a.rec.Outcome, map[string]string{
		outcomeUncertain: ": a change to the target may not have settled; look at the target before running again",
	}[a.rec.Outcome])
	return a, nil
}

var errContended = errors.New("lock held")

// checkStateDir refuses a state directory anyone else could change or
// redirect: whoever can replace deploy.lock, or a directory on the way to it,
// can start a second attempt beside a running one, and whoever can replace a
// run's directory can rewrite its record. The path holds no symlink, so each
// of its components is a directory checked here. The directory must belong to
// this user and be writable by no one else; each ancestor must belong to this
// user or root and be writable by no one else unless it is sticky, like /tmp.
func checkStateDir(dir string) error {
	if err := checkPrivateDir(dir); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if resolved != dir {
		return fmt.Errorf("%s passes through a symlink; give the resolved path %s", dir, resolved)
	}
	euid := uint32(os.Geteuid())
	overflow, inNamespace := unmappedRoot()
	for q := filepath.Dir(dir); ; q = filepath.Dir(q) {
		st, err := os.Lstat(q)
		if err != nil {
			return err
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		trusted := ok && (sys.Uid == euid || sys.Uid == 0 || (q == "/" && inNamespace && sys.Uid == overflow))
		if !trusted || (st.Mode().Perm()&0o022 != 0 && st.Mode()&os.ModeSticky == 0) {
			return fmt.Errorf("others can change %s, on the way to %s", q, dir)
		}
		if q == filepath.Dir(q) {
			return nil
		}
	}
}

// checkPrivateDir requires dir to be a directory, not a symlink, belonging
// to this user and writable by no one else.
func checkPrivateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || int(sys.Uid) != os.Geteuid() || st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be a directory owned by this user and writable by no one else", dir)
	}
	return nil
}

// unmappedRoot is the owner the root directory may show besides root: inside
// a user namespace, as in a build sandbox, the kernel shows owners the
// namespace does not map, such as the real root, as the overflow uid. Only
// the root directory may have it: that uid stands for every unmapped user,
// but whoever owns the root directory controls every path anyway.
func unmappedRoot() (uint32, bool) {
	uidMap, err := os.ReadFile("/proc/self/uid_map")
	if err != nil || strings.Join(strings.Fields(string(uidMap)), " ") == "0 0 4294967295" {
		return 0, false
	}
	data, err := os.ReadFile("/proc/sys/kernel/overflowuid")
	if err != nil {
		return 0, false
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	return uint32(uid), err == nil
}

// acquireLock takes a nonblocking exclusive flock on path.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errContended
		}
		return nil, err
	}
	return f, nil
}

func resolveTools(verb string) (map[string]string, error) {
	if toolPath == "" {
		return nil, errors.New("this build has no pinned tool path; use the packaged deploy")
	}
	var need []string
	switch verb {
	case verbApp:
		need = []string{"git", "nix", "flyctl", "skopeo"}
	case verbDNS:
		need = []string{"git", "cue", "dns"}
	default:
		need = []string{"git", "nix", "cue", "flyctl", "skopeo", "dns"}
	}
	tools := map[string]string{}
	for _, name := range need {
		for _, dir := range filepath.SplitList(toolPath) {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
				tools[name] = p
				break
			}
		}
		if tools[name] == "" {
			return nil, fmt.Errorf("%s is not on the pinned tool path", name)
		}
	}
	return tools, nil
}

// readSecret reads a credential file, trimming terminal newlines.
func readSecret(path, flag string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flag, err)
	}
	s := strings.TrimRight(string(data), "\r\n")
	if s == "" {
		return "", fmt.Errorf("%s: %s is empty", flag, path)
	}
	return s, nil
}

func readSecrets(req request) (secrets, error) {
	var s secrets
	var err error
	if req.verb == verbApp || req.verb == verbConverge {
		if s.flyToken, err = readSecret(req.flyTokenFile, "--fly-token-file"); err != nil {
			return s, err
		}
	}
	if req.verb == verbDNS || req.verb == verbConverge {
		if s.porkbunKey, err = readSecret(req.porkbunKeyFile, "--porkbun-api-key-file"); err != nil {
			return s, err
		}
		s.porkbunSecret, err = readSecret(req.porkbunSecretFile, "--porkbun-secret-key-file")
	}
	return s, err
}

// redact removes every credential from s, in every form the redactor knows.
func (a *attempt) redact(s string) string {
	return a.redactor().text(s)
}

func (a *attempt) redactor() *redactor {
	if a.red == nil {
		a.red = newRedactor(a.secrets.flyToken, a.secrets.porkbunKey, a.secrets.porkbunSecret)
	}
	return a.red
}

// redactor removes credentials in the forms deploy hands them on: each
// secret literally, URL-escaped (query and path escaping), and in standard
// and URL-safe base64, padded and unpadded, both alone and as "x:<secret>",
// the registry auth.
type redactor struct {
	forms   [][]byte // longest first
	longest int
}

func newRedactor(secrets ...string) *redactor {
	r := &redactor{}
	seen := map[string]bool{}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		forms := []string{s, url.QueryEscape(s), url.PathEscape(s)}
		for _, v := range []string{s, "x:" + s} {
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
				forms = append(forms, enc.EncodeToString([]byte(v)))
			}
		}
		for _, f := range forms {
			if !seen[f] {
				seen[f] = true
				r.forms = append(r.forms, []byte(f))
				r.longest = max(r.longest, len(f))
			}
		}
	}
	slices.SortFunc(r.forms, func(x, y []byte) int { return len(y) - len(x) })
	return r
}

// redact returns raw[:end] with every form replaced by [REDACTED]. A form
// that starts before end is replaced whole, even where it runs past end, so
// a caller that cuts raw short can pass the end before which every form is
// complete and never write part of one.
func (r *redactor) redact(raw []byte, end int) []byte {
	type hit struct{ start, end int }
	var hits []hit
	if r != nil {
		for _, f := range r.forms {
			for i := 0; ; {
				j := bytes.Index(raw[i:], f)
				if j < 0 {
					break
				}
				hits = append(hits, hit{i + j, i + j + len(f)})
				i += j + 1
			}
		}
	}
	slices.SortFunc(hits, func(x, y hit) int { return cmp.Or(x.start-y.start, y.end-x.end) })
	var out bytes.Buffer
	pos := 0
	for _, h := range hits {
		switch {
		case h.start >= end:
		case h.start < pos: // overlaps a replaced form: replace the union
			pos = max(pos, h.end)
		default:
			out.Write(raw[pos:h.start])
			out.WriteString("[REDACTED]")
			pos = h.end
		}
	}
	if pos < end {
		out.Write(raw[pos:end])
	}
	return out.Bytes()
}

func (r *redactor) text(s string) string { return string(r.redact([]byte(s), len(s))) }

// slack is how far past a cut a form that starts before it can run.
func (r *redactor) slack() int {
	if r == nil {
		return 0
	}
	return r.longest
}

// create makes runs/<attempt-id>/ and durably writes the first record.
func (a *attempt) create() error {
	runs := filepath.Join(a.req.stateDir, "runs")
	if err := os.Mkdir(runs, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := checkPrivateDir(runs); err != nil {
		return err
	}
	if err := syncDir(a.req.stateDir); err != nil {
		return err
	}
	var id string
	for {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		id = hex.EncodeToString(b)
		err := os.Mkdir(filepath.Join(runs, id), 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if err := syncDir(runs); err != nil {
		return err
	}
	a.runDir = filepath.Join(runs, id)
	// Child logs stay with the record; cleanup removes only work.
	if err := os.Mkdir(filepath.Join(a.runDir, "logs"), 0o700); err != nil {
		return err
	}
	a.work = filepath.Join(a.runDir, "work")
	a.repo = filepath.Join(a.work, "src")
	a.rec = &record{
		SchemaVersion:   1,
		AttemptID:       id,
		Verb:            a.req.verb,
		Name:            strPtr(a.req.name),
		Logs:            "logs",
		DryRun:          a.req.dryRun,
		Source:          a.req.infra,
		RequestedCommit: a.req.rev,
		Platform:        a.system,
		RunnerVersion:   version,
		StartedAt:       timestamp(),
		// An unfinished attempt is uncertain until its final record says
		// otherwise.
		Outcome: outcomeUncertain,
		Phases:  []phaseRecord{},
	}
	return a.persist()
}

func (a *attempt) persist() error {
	data, err := a.rec.marshal()
	if err != nil {
		return err
	}
	return writeAtomic(a.runDir, "record.json", data)
}

func (a *attempt) finish(name string, res result) {
	res.reason = a.redact(res.reason)
	a.rec.setPhase(name, res.outcome, res.reason)
	if res.reason != "" {
		fmt.Fprintf(a.stderr, "deploy: %s %s: %s\n", name, res.outcome, res.reason)
	} else {
		fmt.Fprintf(a.stderr, "deploy: %s %s\n", name, res.outcome)
	}
}

func (a *attempt) steps() []step {
	if a.req.verb == verbApp {
		return []step{
			{name: "source", run: a.source},
			{name: "prepare", run: a.prepareApp},
			{name: "inspect", run: a.inspectApp},
			{name: "upload", run: a.uploadApp},
			{name: "apply", run: a.applyApp},
			{name: "verify", run: a.verifyApp},
		}
	}
	inspect := step{name: "inspect", skip: "dns sync reads the live records itself during apply"}
	apply := step{name: "apply", run: a.applyDNS}
	if a.req.dryRun {
		inspect = step{name: "inspect", run: a.planDNS}
		apply = step{name: "apply", skip: "dry run"}
	}
	return []step{
		{name: "source", run: a.source},
		{name: "prepare", run: a.prepareDNS},
		inspect,
		{name: "upload", skip: "dns-sync uploads nothing"},
		apply,
		{name: "verify", skip: "dns sync's exit status decides completion"},
	}
}

// runPhases runs each phase in order, persisting the record before a phase
// starts (marked uncertain, so a crash mid-phase leaves it uncertain) and
// after it ends. The first failed or uncertain phase, an interruption, or a
// record that cannot be written stops new work: the remaining phases are
// skipped. A recording failure or interruption that keeps a phase from
// starting is recorded as that phase's failure, so the final record always
// explains a nonzero exit. Cleanup always runs, and the final record is always
// attempted.
func (a *attempt) runPhases(ctx context.Context, steps []step) {
	stop := ""
	var unrecorded error // a recording failure no phase records yet
	recordingFailed := func(err error) {
		a.recordFailed = true
		fmt.Fprintf(a.stderr, "deploy: write record: %v\n", err)
		if stop == "" {
			stop, unrecorded = "recording failed", err
		}
	}
	for _, st := range steps {
		switch {
		case unrecorded != nil:
			a.finish(st.name, a.notStarted(st.name, "recording failed: "+unrecorded.Error()))
			unrecorded = nil
			continue
		case stop != "":
			a.finish(st.name, result{outcome: outcomeSkipped, reason: stop})
			continue
		case ctx.Err() != nil:
			stop = context.Cause(ctx).Error()
			a.finish(st.name, a.notStarted(st.name, stop))
			continue
		case st.skip != "":
			a.finish(st.name, result{outcome: outcomeSkipped, reason: st.skip})
		case a.skipAfterInspect != "":
			a.finish(st.name, result{outcome: outcomeSkipped, reason: a.skipAfterInspect})
		default:
			a.rec.setPhase(st.name, outcomeUncertain, "in progress")
			if err := a.persist(); err != nil {
				recordingFailed(err)
				a.finish(st.name, a.notStarted(st.name, "recording failed: "+err.Error()))
				unrecorded = nil
				continue
			}
			res := st.run(ctx)
			a.finish(st.name, res)
			switch {
			case res.outcome != outcomeFailed && res.outcome != outcomeUncertain:
				// An interruption now fails the next phase as not started.
			case ctx.Err() != nil:
				stop = context.Cause(ctx).Error()
			default:
				stop = st.name + " " + res.outcome
			}
		}
		if err := a.persist(); err != nil {
			recordingFailed(err)
		}
	}

	a.rec.setPhase("cleanup", outcomeUncertain, "in progress")
	if !a.recordFailed {
		if err := a.persist(); err != nil {
			recordingFailed(err)
		}
	}
	res := a.cleanup()
	switch {
	case unrecorded != nil && res.outcome == outcomeCompleted:
		res = failed("work directory removed, but the record could not be written: %v", unrecorded)
	case ctx.Err() != nil && res.outcome == outcomeCompleted && overall(a.rec.Phases[:len(a.rec.Phases)-1]) == outcomeCompleted:
		// Every phase before cleanup completed or was skipped, yet the attempt
		// exits with the signal's status: say why.
		res.reason = context.Cause(ctx).Error() + " after every other phase had finished"
	}
	a.finish("cleanup", res)
	a.rec.FinishedAt = strPtr(timestamp())
	a.rec.Outcome = overall(a.rec.Phases)
	if a.rec.Outcome == outcomeCompleted && a.skipped {
		a.rec.Outcome = outcomeSkipped
	}
	if err := a.persist(); err != nil {
		a.recordFailed = true
		fmt.Fprintf(a.stderr, "deploy: write final record: %v\n", err)
	}
}

// notStarted is the result of a phase that never started. It failed, except
// verify after an app's apply completed: the deploy landed and is unverified,
// so uncertain.
func (a *attempt) notStarted(name, cause string) result {
	if name == "verify" && a.req.verb == verbApp && a.phaseOutcome("apply") == outcomeCompleted {
		return result{outcome: outcomeUncertain, reason: "not started: " + cause + "; the completed deploy is unverified"}
	}
	return failed("not started: %s", cause)
}

func (a *attempt) phaseOutcome(name string) string {
	for _, p := range a.rec.Phases {
		if p.Name == name {
			return p.Outcome
		}
	}
	return ""
}

// cleanup removes everything the attempt materialized: the checkout, the
// execution-only build links, temporary credentials and files. Only the
// record remains.
func (a *attempt) cleanup() result {
	if a.auth != nil {
		a.auth.Close()
	}
	if err := removeTree(a.work); err != nil {
		return failed("remove work directory: %v", err)
	}
	return completed()
}

// removeTree is os.RemoveAll that also removes read-only directories a child
// left behind.
func removeTree(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// makeWorkDirs creates the private directories children run in.
func (a *attempt) makeWorkDirs() error {
	for _, d := range []string{a.work, a.repo, a.r.home, a.r.runtime, a.r.tmp, filepath.Join(a.work, "links")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
