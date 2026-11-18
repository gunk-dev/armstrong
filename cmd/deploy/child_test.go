package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestRunner_KillsAfterGrace interrupts a child that ignores SIGTERM: the
// runner must SIGKILL its group once the grace period has passed and reap it.
func TestRunner_KillsAfterGrace(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	ready := filepath.Join(tmp, "ready")
	if err := syscall.Mkfifo(ready, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &runner{
		tools: map[string]string{"ignore-term": filepath.Join(toolDir, "ignore-term")},
		path:  toolDir, home: tmp, runtime: tmp, tmp: tmp,
		grace: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	canceled := make(chan time.Time, 1)
	go func() {
		if f, err := os.Open(ready); err == nil {
			f.Close()
		}
		canceled <- time.Now()
		cancel(errors.New("interrupted by the test"))
	}()

	_, err := r.run(ctx, childSpec{label: "stubborn", tool: "ignore-term", args: []string{ready}, dir: tmp})
	var ce *childErr
	if !errors.As(err, &ce) || ce.kind != childInterrupted {
		t.Fatalf("err = %v, want an interruption", err)
	}
	if elapsed := time.Since(<-canceled); elapsed < r.grace {
		t.Errorf("returned %v after the interruption, before the %v grace period", elapsed, r.grace)
	}
}

// TestStartGate covers the window between the last cancellation check and a
// child's start: a cancellation accepted before it starts nothing, and one
// requested inside it is accepted only once the child has started, so the
// child sees it and is stopped.
func TestStartGate(t *testing.T) {
	t.Parallel()
	newRunner := func(t *testing.T) (*runner, string) {
		tmp := t.TempDir()
		return &runner{
			tools: map[string]string{"ignore-term": filepath.Join(toolDir, "ignore-term")},
			path:  toolDir, home: tmp, runtime: tmp, tmp: tmp,
			grace: 50 * time.Millisecond, gate: &startGate{},
		}, filepath.Join(tmp, "ready")
	}

	t.Run("accepted first", func(t *testing.T) {
		r, ready := newRunner(t)
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		r.gate.close(func() { cancel(errors.New("interrupted by SIGTERM")) })
		var started atomic.Bool
		r.afterStart = func() { started.Store(true) }

		_, err := r.run(ctx, childSpec{label: "stubborn", tool: "ignore-term", args: []string{ready}, dir: r.tmp, mutator: false})
		if !isInterrupted(err) || started.Load() {
			t.Errorf("err %v, started %v; want an interruption and no child", err, started.Load())
		}
	})

	t.Run("requested inside the window", func(t *testing.T) {
		r, ready := newRunner(t)
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		var started atomic.Bool
		r.afterStart = func() { started.Store(true) }
		var startedWhenAccepted bool
		accepted := make(chan struct{})
		r.beforeStart = func() {
			trying := make(chan struct{})
			go func() {
				close(trying)
				r.gate.close(func() {
					startedWhenAccepted = started.Load()
					cancel(errors.New("interrupted by SIGTERM"))
				})
				close(accepted)
			}()
			<-trying
		}

		_, err := r.run(ctx, childSpec{label: "stubborn", tool: "ignore-term", args: []string{ready}, dir: r.tmp})
		<-accepted
		if !isInterrupted(err) || !startedWhenAccepted {
			t.Errorf("err %v, child started before the cancellation was accepted: %v; want an interruption and true", err, startedWhenAccepted)
		}
	})
}

// TestDeadlines covers the documented deadline of every child deploy runs.
func TestDeadlines(t *testing.T) {
	t.Parallel()
	for label, want := range map[string]time.Duration{
		"nix build deploy-demo-image": 30 * time.Minute,
		"flyctl deploy":               10 * time.Minute,
		"skopeo copy":                 10 * time.Minute,
		"git fetch":                   5 * time.Minute,
		"dns sync":                    5 * time.Minute,
		"dns sync --dry-run":          5 * time.Minute,
		"git init":                    time.Minute,
		"git rev-parse":               time.Minute,
		"skopeo inspect":              time.Minute,
		"flyctl machines list":        time.Minute,
		"cue export ./dns/a.dev/":     time.Minute,
	} {
		if got := deadline(label); got != want {
			t.Errorf("deadline(%q) = %v, want %v", label, got, want)
		}
	}
}

func TestChildResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind    childErrKind
		mutator bool
		want    string
	}{
		{childExited, false, outcomeFailed},
		{childExited, true, outcomeUncertain},
		{childTimedOut, false, outcomeFailed},
		{childTimedOut, true, outcomeUncertain},
		{childSignaled, true, outcomeUncertain},
		{childStartFailed, true, outcomeFailed},
		{childInterrupted, false, outcomeFailed},
		{childInterrupted, true, outcomeUncertain},
	} {
		if got := childResult(&childErr{label: "x", kind: tc.kind, err: errors.New("e")}, tc.mutator).outcome; got != tc.want {
			t.Errorf("kind %d, mutator %v: %s, want %s", tc.kind, tc.mutator, got, tc.want)
		}
	}
}
