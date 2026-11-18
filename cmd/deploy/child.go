package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// deadlines are the subprocess deadlines longer than otherTimeout, by the
// first two words of the child's label; every other child gets otherTimeout.
var deadlines = map[string]time.Duration{
	"nix build":     30 * time.Minute,
	"flyctl deploy": 10 * time.Minute,
	"skopeo copy":   10 * time.Minute,
	"git fetch":     5 * time.Minute,
	"dns sync":      5 * time.Minute,
}

const (
	otherTimeout = 60 * time.Second
	killGrace    = 5 * time.Second
	// maxCapture bounds the child output read into memory; every output
	// deploy reads is far smaller.
	maxCapture = 64 << 20
)

func deadline(label string) time.Duration {
	if words := strings.Fields(label); len(words) >= 2 {
		if d, ok := deadlines[words[0]+" "+words[1]]; ok {
			return d
		}
	}
	return otherTimeout
}

// childSpec is one subprocess. Its stdout and stderr, redacted, go to its log
// under the attempt's logs directory, and its stdout is also returned when
// capture is set.
type childSpec struct {
	label   string // names the child in reasons and selects its deadline
	tool    string
	args    []string
	dir     string
	env     []string      // added to the runner's base environment
	stdin   string        // file to read stdin from; empty for /dev/null
	budget  time.Duration // when set, cuts the child's deadline short
	mutator bool          // changes external state; inherits the lock descriptor
	files   []*os.File    // inherited after the lock descriptor, if any
	capture bool
}

type childErrKind int

const (
	childStartFailed childErrKind = iota
	childExited
	childSignaled
	childTimedOut
	childInterrupted
)

type childErr struct {
	label   string
	kind    childErrKind
	code    int
	signal  syscall.Signal
	timeout time.Duration
	err     error
}

func (e *childErr) Error() string {
	switch e.kind {
	case childStartFailed:
		return fmt.Sprintf("%s could not start: %v", e.label, e.err)
	case childExited:
		return fmt.Sprintf("%s exited with status %d", e.label, e.code)
	case childSignaled:
		return fmt.Sprintf("%s was killed by signal %d", e.label, int(e.signal))
	case childTimedOut:
		return fmt.Sprintf("%s timed out after %v", e.label, e.timeout)
	default:
		return fmt.Sprintf("%s %v", e.label, e.err)
	}
}

// runner starts children in a clean environment: the pinned PATH, a private
// HOME, config, cache, runtime and temporary directory, and nothing from the
// invoking environment.
type runner struct {
	tools   map[string]string // tool name -> absolute path on toolPath
	path    string
	home    string
	runtime string
	tmp     string
	lock    *os.File
	grace   time.Duration
	gate    *startGate // nil: only the context stops new children

	logs   string    // directory for child logs; "" keeps none
	seq    int       // children logged so far
	red    *redactor // applied to every log and printed reason
	stderr io.Writer // where a failed child's log is named

	// beforeStart and afterStart, when set, run inside the gate just before
	// and just after a child starts. Tests use them to reach that window.
	beforeStart, afterStart func()
}

// startGate serializes accepting a cancellation with starting a child: once
// close has returned, no child starts, and a child that started before it
// sees the cancellation.
type startGate struct {
	mu     sync.Mutex
	closed bool
}

// close shuts the gate and runs accept, which cancels the attempt, while no
// child is starting.
func (g *startGate) close(accept func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	accept()
}

// fd is the descriptor number a child sees for files[i] of s.
func (s childSpec) fd(i int) int {
	if s.mutator {
		return 4 + i
	}
	return 3 + i
}

func (r *runner) baseEnv() []string {
	return []string{
		"PATH=" + r.path,
		"HOME=" + r.home,
		"XDG_CONFIG_HOME=" + filepath.Join(r.home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(r.home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(r.home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(r.home, ".local", "state"),
		"XDG_RUNTIME_DIR=" + r.runtime,
		"TMPDIR=" + r.tmp,
		"LC_ALL=C",
	}
}

// run starts the child in its own process group and waits for it. On its
// deadline or on interruption the group gets SIGTERM, then SIGKILL after the
// grace period, and the child is always reaped before run returns. A mutator
// inherits the lock descriptor as fd 3, so the lock stays held while it lives,
// even if this process dies.
func (r *runner) run(ctx context.Context, s childSpec) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, &childErr{label: s.label, kind: childInterrupted, err: context.Cause(ctx)}
	}
	path, ok := r.tools[s.tool]
	if !ok {
		return nil, &childErr{label: s.label, kind: childStartFailed, err: fmt.Errorf("%s is not on the tool path", s.tool)}
	}
	// Each stream is drained through a pipe as the child writes, keeping only
	// its first bytes, so the child never blocks on output and a looping
	// child cannot exhaust memory. A form cut at the cap needs room past it
	// to be recognised whole.
	stdout, err := newCapture(maxCapture + r.red.slack())
	if err != nil {
		return nil, &childErr{label: s.label, kind: childStartFailed, err: err}
	}
	stderr, err := newCapture(maxCapture + r.red.slack())
	if err != nil {
		stdout.finish()
		return nil, &childErr{label: s.label, kind: childStartFailed, err: err}
	}
	started, err := r.exec(ctx, s, path, stdout.w, stderr.w)
	out, _ := stdout.finish(), stderr.finish()
	if !started {
		return nil, err
	}
	if log := r.writeLog(s, stdout, stderr, err); err != nil && log != "" {
		fmt.Fprintf(r.errOut(), "deploy: %s; log: %s\n", r.red.text(err.Error()), log)
	}
	if err != nil || !s.capture {
		return nil, err
	}
	if stdout.total > maxCapture {
		return nil, fmt.Errorf("%s: output larger than %d bytes", s.label, maxCapture)
	}
	return out, nil
}

// capture drains one output stream of a child through a pipe, keeping the
// first limit bytes and discarding the rest.
type capture struct {
	r, w  *os.File
	limit int
	buf   []byte
	total int64
	done  chan struct{}
}

func newCapture(limit int) (*capture, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c := &capture{r: r, w: w, limit: limit, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		chunk := make([]byte, 64<<10)
		for {
			n, err := c.r.Read(chunk)
			c.buf = append(c.buf, chunk[:min(n, c.limit-len(c.buf))]...)
			c.total += int64(n)
			if err != nil {
				return
			}
		}
	}()
	return c, nil
}

// finish returns what was kept once every writer has closed the pipe. A
// straggler in the child's group may hold it open indefinitely and no event
// says when it lets go, so after the kill grace period the drain stops.
func (c *capture) finish() []byte {
	c.w.Close()
	grace := time.NewTimer(killGrace)
	defer grace.Stop()
	select {
	case <-c.done:
	case <-grace.C:
		c.r.SetReadDeadline(time.Now())
		<-c.done
	}
	c.r.Close()
	return c.buf
}

// exec starts the child, writing to stdout and stderr, and waits for it.
// started reports whether it ran at all.
func (r *runner) exec(ctx context.Context, s childSpec, path string, stdout, stderr *os.File) (started bool, err error) {
	cmd := &exec.Cmd{
		Path:        path,
		Args:        append([]string{path}, s.args...),
		Dir:         s.dir,
		Env:         append(r.baseEnv(), s.env...),
		Stdout:      stdout,
		Stderr:      stderr,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	if s.mutator {
		cmd.ExtraFiles = []*os.File{r.lock}
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, s.files...)
	if s.stdin != "" {
		f, err := os.Open(s.stdin)
		if err != nil {
			return false, &childErr{label: s.label, kind: childStartFailed, err: err}
		}
		defer f.Close()
		cmd.Stdin = f
	}
	if err := r.start(ctx, cmd); err != nil {
		if ctx.Err() != nil {
			return false, &childErr{label: s.label, kind: childInterrupted, err: context.Cause(ctx)}
		}
		return false, &childErr{label: s.label, kind: childStartFailed, err: err}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timeout := deadline(s.label)
	if s.budget > 0 {
		timeout = min(timeout, s.budget)
	}
	expiry := time.NewTimer(timeout)
	defer expiry.Stop()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-expiry.C:
		r.terminate(cmd.Process.Pid, done)
		return true, &childErr{label: s.label, kind: childTimedOut, timeout: timeout}
	case <-ctx.Done():
		r.terminate(cmd.Process.Pid, done)
		return true, &childErr{label: s.label, kind: childInterrupted, err: context.Cause(ctx)}
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return true, &childErr{label: s.label, kind: childSignaled, signal: ws.Signal()}
			}
			return true, &childErr{label: s.label, kind: childExited, code: exitErr.ExitCode()}
		}
		return true, &childErr{label: s.label, kind: childExited, code: -1, err: waitErr}
	}
	return true, nil
}

// writeLog writes the child's redacted command, outcome and output to the
// next file under the logs directory, and returns its path, or "" when the
// runner keeps no logs or the log could not be written.
func (r *runner) writeLog(s childSpec, stdout, stderr *capture, result error) string {
	if r.logs == "" {
		return ""
	}
	r.seq++
	path := filepath.Join(r.logs, fmt.Sprintf("%02d-%s.log", r.seq, logName(s.label)))
	outcome := "exited with status 0"
	if result != nil {
		outcome = result.Error()
	}
	var b bytes.Buffer
	b.WriteString(r.red.text(fmt.Sprintf("$ %s %s\n%s\n", s.tool, strings.Join(s.args, " "), outcome)))
	for _, stream := range []struct {
		name string
		c    *capture
	}{{"stdout", stdout}, {"stderr", stderr}} {
		data, cut := r.red.capped(stream.c.buf, stream.c.total > int64(len(stream.c.buf)), maxCapture)
		fmt.Fprintf(&b, "\n--- %s ---\n%s", stream.name, data)
		if cut {
			fmt.Fprintf(&b, "\n[output cut at %d bytes]\n", maxCapture)
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		fmt.Fprintf(r.errOut(), "deploy: write log: %v\n", err)
		return ""
	}
	return path
}

// capped redacts kept output, then cuts it to limit bytes. When the capture
// was cut, a form may run past what was kept, so only forms starting at least
// slack-1 bytes before its end count; nothing after them is written.
func (r *redactor) capped(kept []byte, captureCut bool, limit int) (data []byte, cut bool) {
	end := len(kept)
	if captureCut {
		end = max(0, len(kept)-max(0, r.slack()-1))
	}
	data = r.redact(kept, end)
	if len(data) > limit {
		return data[:limit], true
	}
	return data, captureCut
}

// logName turns a child's label into a file name: "cue vet ./dns/a.dev/"
// becomes "cue-vet-dns-a.dev".
func logName(label string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(strings.ReplaceAll(label, "./", "")) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' {
			b.WriteRune(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func (r *runner) errOut() io.Writer {
	if r.stderr == nil {
		return io.Discard
	}
	return r.stderr
}

// start starts cmd unless the attempt is cancelled, holding the gate shut so
// no cancellation is accepted between the check and the start.
func (r *runner) start(ctx context.Context, cmd *exec.Cmd) error {
	if r.gate != nil {
		r.gate.mu.Lock()
		defer r.gate.mu.Unlock()
		if r.gate.closed {
			return context.Canceled
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.beforeStart != nil {
		r.beforeStart()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if r.afterStart != nil {
		r.afterStart()
	}
	return nil
}

// terminate stops the process group led by pgid and reaps the leader.
func (r *runner) terminate(pgid int, done <-chan error) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	grace := time.NewTimer(r.grace)
	defer grace.Stop()
	select {
	case <-done:
		// The leader is reaped. Whatever else is left in its group gets the
		// rest of the grace period.
		if syscall.Kill(-pgid, 0) == nil {
			<-grace.C
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	case <-grace.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}
}

// childResult maps a child's error to a phase result. Uncertain means one
// thing: a change to the deployment target started and was neither confirmed
// complete nor confirmed not to have happened. So only a child that changes
// the target (fly deploy, the real dns sync) makes its phase uncertain, when
// it is interrupted, times out or exits nonzero; any other child's failure,
// interruptions included, is a known failure.
func childResult(err error, changesTarget bool) result {
	if err == nil {
		return result{outcome: outcomeCompleted}
	}
	var ce *childErr
	if changesTarget && errors.As(err, &ce) && ce.kind != childStartFailed {
		return result{outcome: outcomeUncertain, reason: err.Error()}
	}
	return result{outcome: outcomeFailed, reason: err.Error()}
}

func isInterrupted(err error) bool {
	var ce *childErr
	return errors.As(err, &ce) && ce.kind == childInterrupted
}
