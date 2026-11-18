package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestLock_ContentionExits75(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	if err := os.MkdirAll(h.state, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := acquireLock(filepath.Join(h.state, "deploy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	o := h.app()
	if o.code != exitContention || o.rec != nil || len(h.calls()) != 0 {
		t.Errorf("exit %d, record %v, %d calls; want 75, none, none\n%s", o.code, o.rec != nil, len(h.calls()), o.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.state, "runs")); !os.IsNotExist(err) {
		t.Error("a contended attempt created a run")
	}
}

// TestStateDirMustBePrivate refuses a state or runs directory others can
// write, or a state directory under one: they could replace deploy.lock under
// a running attempt, or rewrite its record. The harness's own state directory
// sits under the sticky temporary directory, which every other test accepts.
func TestStateDirMustBePrivate(t *testing.T) {
	for _, open := range []string{"state", "state/runs", "shared", "link"} {
		t.Run(open, func(t *testing.T) {
			h := newHarness(t)
			h.appFixture()
			switch open {
			case "shared":
				h.state = filepath.Join(h.root, "shared", "state")
			case "link":
				// A symlink on the way could be retargeted after the check.
				if err := os.Symlink(h.root, filepath.Join(h.root, "link")); err != nil {
					t.Fatal(err)
				}
				h.state = filepath.Join(h.root, "link", "state")
			}
			if err := os.MkdirAll(filepath.Join(h.state, "runs"), 0o700); err != nil {
				t.Fatal(err)
			}
			if open != "link" {
				if err := os.Chmod(filepath.Join(h.root, open), 0o770); err != nil {
					t.Fatal(err)
				}
			}

			o := h.app()
			if o.code != exitFailure || o.rec != nil || len(h.calls()) != 0 {
				t.Errorf("exit %d, record %v, %d calls; want 1, none, none\n%s", o.code, o.rec != nil, len(h.calls()), o.stderr)
			}
		})
	}
}

// TestSignalDuringCredentialRead interrupts deploy while it waits on a
// credential pipe nothing writes to: the signal still ends the attempt.
func TestSignalDuringCredentialRead(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	fifo := filepath.Join(h.root, "token-pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	args := h.appArgs()
	args[len(args)-1] = fifo
	cmd := h.command(args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Returns once deploy has opened the pipe, so it is reading.
	writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != exitSIGINT {
		t.Errorf("exit %d, want %d", code, exitSIGINT)
	}
	if _, err := os.Stat(h.state); !os.IsNotExist(err) {
		t.Error("an attempt interrupted before its lock touched the state directory")
	}
}

// TestRecordPathMustBePrinted starts no work when the record's path cannot
// be printed, whether stdout refuses writes or its reader has gone, and in
// both cases still cleans up and finishes the record.
func TestRecordPathMustBePrinted(t *testing.T) {
	for _, stdout := range []string{"read-only", "closed pipe"} {
		t.Run(stdout, func(t *testing.T) { recordPathUnprintable(t, stdout) })
	}
}

func recordPathUnprintable(t *testing.T, stdout string) {
	h := newHarness(t)
	h.appFixture()
	cmd := h.command(h.appArgs()...)
	out, err := os.Open(os.DevNull)
	if stdout == "closed pipe" {
		var r *os.File
		r, out, err = os.Pipe()
		if err == nil {
			r.Close()
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd.Stdout = out

	err = cmd.Run()
	if code := cmd.ProcessState.ExitCode(); code != exitFailure {
		t.Errorf("exit %d (%v), want 1", code, err)
	}
	if calls := h.callNames(); len(calls) != 0 {
		t.Errorf("ran %q", calls)
	}
	records, _ := filepath.Glob(filepath.Join(h.state, "runs", "*", "record.json"))
	if len(records) != 1 {
		t.Fatalf("records %v", records)
	}
	var rec record
	if err := readJSON(records[0], &rec); err != nil {
		t.Fatal(err)
	}
	o := outcome{rec: &rec}
	if got, want := o.phases(), phaseNames("failed", "skipped", "skipped", "skipped", "skipped", "skipped", "completed"); !slices.Equal(got, want) {
		t.Errorf("phases %q, want %q", got, want)
	}
}

// blockedDeploy starts deploy with its flyctl deploy blocked on the release
// FIFO, and returns once the fake deploy has started, with the fake's PID.
func blockedDeploy(t *testing.T, h *harness) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, <-chan error, int) {
	t.Helper()
	for _, fifo := range []string{"ready", "release"} {
		if err := syscall.Mkfifo(filepath.Join(h.root, fifo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.sc.Deploy.Block = true
	cmd := h.command(h.appArgs()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ready := make(chan int, 1)
	go func() {
		f, err := os.Open(filepath.Join(h.root, "ready"))
		if err != nil {
			return
		}
		data, _ := io.ReadAll(f)
		f.Close()
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		ready <- pid
	}()
	select {
	case pid := <-ready:
		return cmd, &stdout, &stderr, done, pid
	case err := <-done:
		t.Fatalf("deploy exited before its deploy started: %v\n%s", err, stderr.String())
	}
	panic("unreachable")
}

// TestLock_SurvivesParentDeath kills deploy while flyctl deploy runs: the
// mutating child still holds the lock descriptor it inherited, so no other
// attempt can start until it exits.
func TestLock_SurvivesParentDeath(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	cmd, _, _, done, _ := blockedDeploy(t, h)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done

	if o := h.app(); o.code != exitContention {
		t.Errorf("attempt while the orphaned deploy runs: exit %d, want 75\n%s", o.code, o.stderr)
	}

	release, err := os.OpenFile(filepath.Join(h.root, "release"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	release.Close()
	// Blocks until the orphan exits and its descriptor closes.
	lock, err := os.OpenFile(filepath.Join(h.state, "deploy.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
}

func TestSignals(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{
		{syscall.SIGINT, exitSIGINT},
		{syscall.SIGTERM, exitSIGTERM},
		{syscall.SIGHUP, exitSIGHUP},
	} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			h := newHarness(t)
			h.appFixture()
			cmd, stdout, stderr, done, pid := blockedDeploy(t, h)

			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			err := <-done
			o := h.finish(err, stdout, stderr)
			t.Logf("deploy printed:\n%s%s", o.stdout, o.stderr)
			namedLog(t, o.stderr, "flyctl-deploy")
			// The interrupted deploy may have landed, so apply is uncertain.
			wantRun(t, o, tc.code, outcomeUncertain, phaseNames("completed", "completed", "completed", "completed", "uncertain", "skipped", "completed")...)
			wantCleanedUp(t, o)
			// deploy terminated and reaped its child before exiting.
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("fake flyctl deploy (pid %d) outlived deploy: %v", pid, err)
			}
		})
	}
}

// TestRecordingFailureStopsNewWork makes the record impossible to replace
// during prepare: nothing after it runs, but cleanup still does.
func TestRecordingFailureStopsNewWork(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	out := h.sc.Outputs["deploy-demo-image"]
	out.BreakRecord = true
	h.sc.Outputs["deploy-demo-image"] = out

	o := h.run(h.appArgs()...)
	if o.code != exitFailure {
		t.Errorf("exit %d, want 1\n%s", o.code, o.stderr)
	}
	for _, name := range h.callNames() {
		if strings.HasPrefix(name, "skopeo") || strings.HasPrefix(name, "flyctl") {
			t.Errorf("%s ran after the record could not be written", name)
		}
	}
	runs, _ := filepath.Glob(filepath.Join(h.state, "runs", "*"))
	if len(runs) != 1 {
		t.Fatalf("runs %v", runs)
	}
	entries, _ := os.ReadDir(runs[0])
	if len(entries) != 2 || entries[0].Name() != "logs" || entries[1].Name() != "record.json" {
		t.Errorf("run directory holds %v, want only logs and the broken record.json", entries)
	}
}
