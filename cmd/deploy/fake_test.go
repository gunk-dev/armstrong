package main

import (
	"archive/tar"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// The test binary doubles as every tool deploy runs: TestMain hands control
// to runFake when it is invoked through one of the symlinks on the pinned
// tool path. nix, flyctl and skopeo are scripted fakes that accept exactly the
// invocations deploy is meant to make, reject anything else, and act on a
// fake Fly app and registry. git and cue run the real tools, and dns runs the
// real dns binary pointed at a fake Porkbun API. Every invocation is logged
// with its arguments and environment, and the credential-bearing ones print
// what they received, as canaries for leaks.
//
// Children run in a clean environment, so a fake finds its test's files by
// walking up from its working directory, which deploy always places under the
// test's state directory, to the directory holding scenarioFile.

const scenarioFile = "deploy-fake.json"

var fakeTools = []string{"git", "cue", "dns", "nix", "flyctl", "skopeo"}

// scenario tells the fakes how to behave in one test.
type scenario struct {
	System        string
	StoreDir      string                // every build output lies here, as with real nix
	Outputs       map[string]fakeOutput // by flake attribute
	ExtraPackages []string              // extra packages.<system> names returned by nix eval
	EvalFail      bool                  // nix eval fails
	EvalTouchLock bool                  // nix eval rewrites the checkout's flake.lock
	Deploy        deployBehaviour
	AppDeploys    map[string]deployBehaviour // per Fly app override of Deploy
	CopyExit      int                        // skopeo copy's exit status after the push lands
	CorruptPush   bool                       // the registry stores manifest bytes other than the archive's
	ListFail      bool                       // flyctl list commands fail
	AppListFail   map[string]bool            // per Fly app override of ListFail
	PorkbunBase   string
}

type fakeOutput struct {
	Path        string
	Fail        bool // the build fails, as when the image's smoke check fails
	TouchLock   bool // the build rewrites the checkout's flake.lock
	BreakRecord bool // the build leaves record.json impossible to replace
}

type deployBehaviour struct {
	// Effect is what the deploy does to the app beyond moving every Machine to
	// the image: "replace" gives every Machine a new ID, "add-machine" adds
	// one, "drop-volume" destroys a volume, "stale" leaves a started Machine
	// on its old image, "skip-idle" leaves every Machine that is not started
	// on its old image, "stop-all" stops every Machine, "leave-replacing"
	// leaves the first Machine between states, "transient-extra" shows the
	// first list after it an extra Machine still being destroyed.
	Effect      string
	Exit        int  // exit status after the effect lands: nonzero is a lost response
	Lag         int  // machines list calls that still show the Machines as before
	Block       bool // rendezvous on the ready and release FIFOs before landing
	BreakLists  bool // list calls fail once the deploy has landed...
	ListsBefore int  // ...after this many more succeed
	HangLists   bool // and hang until killed instead of failing
}

// world is the fake Fly app (or apps, when Apps is set) and registry, which
// the fakes change.
type world struct {
	App       string
	Machines  []fakeMachine
	Volumes   []fakeVolume
	Registry  map[string][]byte // "<repository>:<tag>" -> manifest
	Pending   []fakeMachine     // Machines the deploy left, shown after Lag lists
	Lag       int
	Broken    bool // list calls fail once ListsLeft runs out
	ListsLeft int
	Apps      map[string]*appState // multi-app worlds, keyed by Fly app name
}

type appState struct {
	Machines  []fakeMachine
	Volumes   []fakeVolume
	Pending   []fakeMachine
	Lag       int
	Broken    bool
	ListsLeft int
}

type fakeMachine struct{ ID, State, Digest string }

type fakeVolume struct{ ID, Attached string }

// call is one logged invocation.
type call struct {
	Tool string
	Args []string
	Env  []string
	Dir  string
	Info map[string]string
}

func (c call) env(key string) (string, bool) {
	for _, kv := range c.Env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v, true
		}
	}
	return "", false
}

func fakeTool() (int, bool) {
	name := filepath.Base(os.Args[0])
	if name == "ignore-term" {
		// Swallows every SIGTERM, says so by opening and closing the FIFO
		// named by its argument, and runs until killed.
		terms := make(chan os.Signal, 1)
		signal.Notify(terms, syscall.SIGTERM)
		if ready, err := os.OpenFile(os.Args[1], os.O_WRONLY, 0); err == nil {
			ready.Close()
		}
		for range terms {
		}
		return 0, true
	}
	if !slices.Contains(fakeTools, name) {
		return 0, false
	}
	f, err := newFake(name, os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake %s: %v\n", name, err)
		return 98, true
	}
	return f.run(), true
}

type fake struct {
	tool string
	args []string
	dir  string
	sc   scenario
	info map[string]string
}

func newFake(tool string, args []string) (*fake, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, scenarioFile)); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("no %s above %s", scenarioFile, cwd)
		}
		dir = parent
	}
	f := &fake{tool: tool, args: args, dir: dir, info: map[string]string{}}
	return f, readJSON(filepath.Join(dir, scenarioFile), &f.sc)
}

func (f *fake) run() int {
	switch f.tool {
	case "git", "cue":
		f.record()
		return f.exec(f.realTool(f.tool))
	case "dns":
		f.record()
		f.canary(true)
		return f.exec(f.realTool("dns"), "PORKBUN_API_BASE="+f.sc.PorkbunBase)
	case "nix":
		return f.nix()
	case "flyctl":
		return f.flyctl()
	default:
		return f.skopeo()
	}
}

func (f *fake) record() {
	cwd, _ := os.Getwd()
	line, _ := json.Marshal(call{Tool: f.tool, Args: f.args, Env: os.Environ(), Dir: cwd, Info: f.info})
	log, err := os.OpenFile(filepath.Join(f.dir, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer log.Close()
	log.Write(append(line, '\n'))
}

func (f *fake) unexpected() int {
	f.info["unexpected"] = "true"
	f.record()
	fmt.Fprintf(os.Stderr, "fake %s: unexpected invocation %q\n", f.tool, f.args)
	return 97
}

// canary prints every credential the fake received, as a careless tool
// might.
func (f *fake) canary(toStdout bool) {
	var vals []string
	for _, k := range []string{"FLY_API_TOKEN", "PORKBUN_API_KEY", "PORKBUN_SECRET_KEY"} {
		if v := os.Getenv(k); v != "" {
			vals = append(vals, v)
		}
	}
	if p := f.info["authPass"]; p != "" {
		vals = append(vals, p)
	}
	for _, v := range slices.Clone(vals) {
		vals = append(vals, encodedForms(v)...)
	}
	line := "CANARY " + strings.Join(vals, " ") + "\n"
	os.Stderr.WriteString(line)
	if toStdout {
		os.Stdout.WriteString(line)
	}
}

// encodedForms are the encodings of a credential the logs must not show:
// URL-escaped, and standard and URL-safe base64, padded and unpadded, alone
// and as the registry auth "x:<secret>".
func encodedForms(secret string) []string {
	forms := []string{url.QueryEscape(secret), url.PathEscape(secret)}
	for _, v := range []string{secret, "x:" + secret} {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			forms = append(forms, enc.EncodeToString([]byte(v)))
		}
	}
	return forms
}

func (f *fake) realTool(name string) string {
	var real map[string]string
	readJSON(filepath.Join(filepath.Dir(os.Args[0]), "real-tools.json"), &real)
	return real[name]
}

func (f *fake) exec(path string, extraEnv ...string) int {
	if path == "" {
		fmt.Fprintf(os.Stderr, "fake %s: the real tool is not installed\n", f.tool)
		return 98
	}
	err := syscall.Exec(path, append([]string{path}, f.args...), append(os.Environ(), extraEnv...))
	fmt.Fprintf(os.Stderr, "fake %s: exec: %v\n", f.tool, err)
	return 98
}

func (f *fake) loadWorld() *world {
	var w world
	if err := readJSON(filepath.Join(f.dir, "world.json"), &w); err != nil {
		panic(err)
	}
	if w.Registry == nil {
		w.Registry = map[string][]byte{}
	}
	return &w
}

func (f *fake) saveWorld(w *world) {
	if err := writeJSON(filepath.Join(f.dir, "world.json"), w); err != nil {
		panic(err)
	}
}

// nix accepts only
//
//	nix build --extra-experimental-features 'nix-command flakes'
//	  --no-update-lock-file --no-write-lock-file
//	  --option accept-flake-config false --out-link <link>
//	  git+file://<checkout>?rev=<rev>#packages.<system>.<attr>
//
// and
//
//	nix eval --extra-experimental-features 'nix-command flakes'
//	  --no-update-lock-file --no-write-lock-file
//	  --option accept-flake-config false --json --apply <expr>
//	  git+file://<checkout>?rev=<rev>#.
func (f *fake) nix() int {
	evalHead := []string{"eval", "--extra-experimental-features", "nix-command flakes",
		"--no-update-lock-file", "--no-write-lock-file", "--option", "accept-flake-config", "false",
		"--json", "--apply", fmt.Sprintf(`f: builtins.attrNames ((f.packages or {}).%q or {})`, f.sc.System)}
	if len(f.args) == len(evalHead)+1 && slices.Equal(f.args[:len(evalHead)], evalHead) {
		return f.nixEval(f.args[len(evalHead)])
	}
	head := []string{"build", "--extra-experimental-features", "nix-command flakes",
		"--no-update-lock-file", "--no-write-lock-file", "--option", "accept-flake-config", "false", "--out-link"}
	if len(f.args) != len(head)+2 || !slices.Equal(f.args[:len(head)], head) {
		return f.unexpected()
	}
	link, installable := f.args[len(head)], f.args[len(head)+1]
	ref, attrPath, _ := strings.Cut(installable, "#")
	u, err := url.Parse(ref)
	attr, ok := strings.CutPrefix(attrPath, "packages."+f.sc.System+".")
	if err != nil || u.Scheme != "git+file" || !ok {
		return f.unexpected()
	}
	checkout := u.Path
	marker, _ := os.ReadFile(filepath.Join(checkout, "marker"))
	f.info["attr"], f.info["rev"], f.info["marker"] = attr, u.Query().Get("rev"), string(marker)
	f.record()
	f.canary(true)

	out, ok := f.sc.Outputs[attr]
	if !ok {
		fmt.Fprintf(os.Stderr, "error: flake does not provide attribute %s\n", attrPath)
		return 1
	}
	if !strings.HasPrefix(out.Path, f.sc.StoreDir+"/") {
		fmt.Fprintf(os.Stderr, "fake nix: output %s is not in the store\n", out.Path)
		return 1
	}
	if out.TouchLock {
		lock, _ := os.OpenFile(filepath.Join(checkout, "flake.lock"), os.O_APPEND|os.O_WRONLY, 0)
		lock.WriteString("\n")
		lock.Close()
	}
	if out.BreakRecord {
		record := filepath.Join(filepath.Dir(filepath.Dir(checkout)), "record.json")
		os.Remove(record)
		os.Mkdir(record, 0o700)
	}
	if out.Fail {
		fmt.Fprintln(os.Stderr, "error: builder for smoke check failed")
		return 1
	}
	if err := os.Symlink(out.Path, link); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func (f *fake) nixEval(installable string) int {
	ref, frag, ok := strings.Cut(installable, "#")
	u, err := url.Parse(ref)
	if !ok || frag != "." || err != nil || u.Scheme != "git+file" {
		return f.unexpected()
	}
	checkout := u.Path
	marker, _ := os.ReadFile(filepath.Join(checkout, "marker"))
	f.info["rev"], f.info["marker"] = u.Query().Get("rev"), string(marker)
	f.record()
	f.canary(false)

	if f.sc.EvalTouchLock {
		lock, err := os.OpenFile(filepath.Join(checkout, "flake.lock"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if _, err := lock.WriteString("\n"); err != nil {
			lock.Close()
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := lock.Close(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if f.sc.EvalFail {
		fmt.Fprintln(os.Stderr, "error: evaluation of flake failed")
		return 1
	}
	var names []string
	for k := range f.sc.Outputs {
		names = append(names, k)
	}
	names = append(names, f.sc.ExtraPackages...)
	slices.Sort(names)
	if names == nil {
		names = []string{}
	}
	return writeStdout(names)
}

func (f *fake) deployBehaviour(app string) deployBehaviour {
	if b, ok := f.sc.AppDeploys[app]; ok {
		return b
	}
	return f.sc.Deploy
}

func (f *fake) listFails(app string) bool {
	if fail, ok := f.sc.AppListFail[app]; ok {
		return fail
	}
	return f.sc.ListFail
}

func (w *world) stateFor(app string) (*appState, func()) {
	if w.Apps != nil {
		st, ok := w.Apps[app]
		if !ok {
			return nil, nil
		}
		return st, func() {}
	}
	if app != w.App {
		return nil, nil
	}
	st := &appState{
		Machines:  w.Machines,
		Volumes:   w.Volumes,
		Pending:   w.Pending,
		Lag:       w.Lag,
		Broken:    w.Broken,
		ListsLeft: w.ListsLeft,
	}
	return st, func() {
		w.Machines = st.Machines
		w.Volumes = st.Volumes
		w.Pending = st.Pending
		w.Lag = st.Lag
		w.Broken = st.Broken
		w.ListsLeft = st.ListsLeft
	}
}

// flyctl accepts only machines list, volumes list and deploy, in the forms
// deploy uses.
func (f *fake) flyctl() int {
	a := f.args
	w := f.loadWorld()
	f.info["token"] = os.Getenv("FLY_API_TOKEN")
	switch {
	case len(a) == 5 && (a[0] == "machines" || a[0] == "volumes") && a[1] == "list" && a[2] == "--app" && a[4] == "--json":
		f.record()
		f.canary(false)
		app := a[3]
		st, sync := w.stateFor(app)
		beh := f.deployBehaviour(app)
		if st != nil && st.Broken && st.ListsLeft <= 0 && beh.HangLists {
			time.Sleep(time.Hour) // until deploy kills it at its deadline
		}
		if st == nil || f.listFails(app) || (st.Broken && st.ListsLeft <= 0) {
			fmt.Fprintln(os.Stderr, "Error: Could not find App")
			return 1
		}
		if st.Broken {
			st.ListsLeft--
			sync()
			f.saveWorld(w)
		}
		if a[0] == "volumes" {
			return writeStdout(flyVolumes(st.Volumes))
		}
		if st.Pending != nil {
			if st.Lag > 0 {
				st.Lag--
			} else {
				st.Machines, st.Pending = st.Pending, nil
			}
			sync()
			f.saveWorld(w)
		}
		return writeStdout(flyMachines(app, st.Machines))
	case len(a) == 7 && a[0] == "deploy" && a[1] == "--config" && a[3] == "--app" && a[5] == "--image":
		f.record()
		f.canary(true)
		return f.deploy(w, a[2], a[4], a[6])
	}
	return f.unexpected()
}

func (f *fake) deploy(w *world, config, app, image string) int {
	st, sync := w.stateFor(app)
	var cfg struct {
		App string `json:"app"`
	}
	if data, err := os.ReadFile(config); err != nil || json.Unmarshal(data, &cfg) != nil || cfg.App != app || st == nil {
		fmt.Fprintln(os.Stderr, "Error: bad config")
		return 1
	}
	digest, ok := strings.CutPrefix(image, "registry.fly.io/"+app+"@")
	if !ok || !w.hasManifest(app, digest) {
		fmt.Fprintln(os.Stderr, "Error: manifest unknown")
		return 1
	}
	beh := f.deployBehaviour(app)
	if beh.Block {
		f.block()
	}
	machines, volumes := landDeploy(st, beh.Effect, digest)
	switch {
	case beh.Effect == "transient-extra":
		st.Machines = append(slices.Clone(machines), fakeMachine{ID: "m0-old", State: "destroying", Digest: st.Machines[0].Digest})
		st.Pending, st.Lag = machines, 1
	case beh.Lag > 0:
		st.Pending, st.Lag = machines, beh.Lag
	default:
		st.Machines = machines
	}
	st.Volumes = volumes
	st.Broken, st.ListsLeft = beh.BreakLists, beh.ListsBefore
	sync()
	f.saveWorld(w)
	return beh.Exit
}

func (w *world) hasManifest(app, digest string) bool {
	prefix := app + ":"
	for k, m := range w.Registry {
		if strings.HasPrefix(k, prefix) && "sha256:"+sha256Hex(m) == digest {
			return true
		}
	}
	return false
}

func landDeploy(st *appState, effect, digest string) ([]fakeMachine, []fakeVolume) {
	ms, vs := slices.Clone(st.Machines), slices.Clone(st.Volumes)
	for i := range ms {
		ms[i].Digest = digest
	}
	switch effect {
	case "replace":
		for i := range ms {
			old := ms[i].ID
			ms[i].ID = old + "-new"
			for j := range vs {
				if vs[j].Attached == old {
					vs[j].Attached = ms[i].ID
				}
			}
		}
	case "add-machine":
		ms = append(ms, fakeMachine{ID: "m9", State: "started", Digest: digest})
	case "drop-volume":
		vs = vs[1:]
	case "stale":
		for i := range ms {
			if ms[i].State == "started" {
				ms[i].Digest = st.Machines[i].Digest
				break
			}
		}
	case "skip-idle":
		for i := range ms {
			if ms[i].State != "started" {
				ms[i].Digest = st.Machines[i].Digest
			}
		}
	case "stop-all":
		for i := range ms {
			ms[i].State = "stopped"
		}
	case "leave-replacing":
		ms[0].State = "replacing"
	}
	return ms, vs
}

// flyMachines and flyVolumes render the shape `flyctl ... list --json`
// prints, trimmed to a few fields around the ones deploy reads.
func flyMachines(app string, ms []fakeMachine) []map[string]any {
	out := []map[string]any{}
	for _, m := range ms {
		out = append(out, map[string]any{
			"id": m.ID, "name": "machine-" + m.ID, "state": m.State, "region": "ord",
			"image_ref": map[string]any{"registry": "registry.fly.io", "repository": app, "tag": "", "digest": m.Digest},
			"config":    map[string]any{"image": "registry.fly.io/" + app + "@" + m.Digest},
		})
	}
	return out
}

func flyVolumes(vs []fakeVolume) []map[string]any {
	out := []map[string]any{}
	for _, v := range vs {
		var attached any
		if v.Attached != "" {
			attached = v.Attached
		}
		out = append(out, map[string]any{"id": v.ID, "name": "data", "state": "created", "size_gb": 1, "region": "ord", "attached_machine_id": attached})
	}
	return out
}

// skopeo accepts only copy and inspect --raw, in the forms deploy uses.
func (f *fake) skopeo() int {
	a := f.args
	if len(a) < 7 || a[0] != "--policy" || a[2] != "--registries.d" || a[4] != "--tmpdir" {
		return f.unexpected()
	}
	if policy, err := os.ReadFile(a[1]); err == nil && strings.Contains(string(policy), "insecureAcceptAnything") {
		f.info["policy"] = "ok"
	}
	f.info["tmpdir"] = a[5]
	f.readAuth()
	rest := a[6:]
	const prefix = "docker://registry.fly.io/"
	w := f.loadWorld()
	switch {
	case len(rest) == 4 && rest[0] == "copy" && rest[1] == "--preserve-digests" && strings.HasPrefix(rest[2], "oci-archive:") && strings.HasPrefix(rest[3], prefix):
		f.record()
		f.canary(true)
		if f.info["authUser"] != "x" || f.info["authPass"] == "" {
			fmt.Fprintln(os.Stderr, "unauthorized")
			return 1
		}
		manifest, err := archiveManifest(strings.TrimPrefix(rest[2], "oci-archive:"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if f.sc.CorruptPush {
			manifest = append(manifest, '\n')
		}
		w.Registry[strings.TrimPrefix(rest[3], prefix)] = manifest
		f.saveWorld(w)
		return f.sc.CopyExit
	case len(rest) == 3 && rest[0] == "inspect" && rest[1] == "--raw" && strings.HasPrefix(rest[2], prefix):
		f.record()
		f.canary(false)
		manifest, ok := w.Registry[strings.TrimPrefix(rest[2], prefix)]
		if !ok {
			fmt.Fprintln(os.Stderr, "manifest unknown")
			return 1
		}
		os.Stdout.Write(manifest)
		return 0
	}
	return f.unexpected()
}

func (f *fake) readAuth() {
	path := os.Getenv("REGISTRY_AUTH_FILE")
	st, err := os.Stat(path)
	if path == "" || err != nil {
		return
	}
	f.info["authFile"] = path
	f.info["authLink"], _ = os.Readlink(path)
	f.info["authMode"] = fmt.Sprintf("%o", st.Mode().Perm())
	var auth struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &auth)
	decoded, _ := base64.StdEncoding.DecodeString(auth.Auths["registry.fly.io"].Auth)
	f.info["authUser"], f.info["authPass"], _ = strings.Cut(string(decoded), ":")
}

// block tells the test the deploy has started, by writing this process's PID
// to the ready FIFO, then waits until the test opens and closes the release
// FIFO.
func (f *fake) block() {
	if ready, err := os.OpenFile(filepath.Join(f.dir, "ready"), os.O_WRONLY, 0); err == nil {
		fmt.Fprintf(ready, "%d\n", os.Getpid())
		ready.Close()
	}
	if release, err := os.Open(filepath.Join(f.dir, "release")); err == nil {
		io.Copy(io.Discard, release)
		release.Close()
	}
}

// archiveManifest returns the manifest an OCI archive's index points at, as
// skopeo copy --preserve-digests would push it.
func archiveManifest(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries := map[string][]byte{}
	tr := tar.NewReader(file)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		entries[h.Name], _ = io.ReadAll(tr)
	}
	var index struct {
		Manifests []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(entries["index.json"], &index); err != nil || len(index.Manifests) == 0 {
		return nil, fmt.Errorf("no manifest in %s", path)
	}
	return entries["blobs/sha256/"+strings.TrimPrefix(index.Manifests[0].Digest, "sha256:")], nil
}

func writeStdout(v any) int {
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		return 1
	}
	return 0
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
