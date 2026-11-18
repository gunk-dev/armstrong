package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRunPhases_InterruptedBetweenPhases interrupts an app attempt as a phase
// completes. Before apply, the next phase is recorded as failed, not skipped,
// so the attempt cannot read as completed; after a completed apply, verify
// never runs and the landed deploy is unverified, so uncertain.
func TestRunPhases_InterruptedBetweenPhases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		after   string
		want    []string
		outcome string
	}{
		{"upload", phaseNames("completed", "completed", "completed", "completed", "failed", "skipped", "completed"), outcomeFailed},
		{"apply", phaseNames("completed", "completed", "completed", "completed", "completed", "uncertain", "completed"), outcomeUncertain},
	} {
		runDir := t.TempDir()
		a := &attempt{req: request{verb: verbApp}, runDir: runDir, work: filepath.Join(runDir, "work"), stderr: io.Discard, rec: &record{Phases: []phaseRecord{}}}
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		var steps []step
		for _, name := range []string{"source", "prepare", "inspect", "upload", "apply", "verify"} {
			steps = append(steps, step{name: name, run: func(context.Context) result {
				if name == tc.after {
					cancel(errors.New("interrupted by SIGTERM"))
				}
				return completed()
			}})
		}

		a.runPhases(ctx, steps)
		var got []string
		for _, p := range a.rec.Phases {
			got = append(got, p.Name+" "+p.Outcome)
		}
		if !slices.Equal(got, tc.want) || a.rec.Outcome != tc.outcome {
			t.Errorf("interrupted after %s: phases %q, outcome %s; want %q, %s", tc.after, got, a.rec.Outcome, tc.want, tc.outcome)
		}
	}
}

// TestRunPhases_InterruptedAfterTheLastPhase interrupts as the last work
// phase completes: the attempt did complete, and cleanup's reason records the
// signal that sets the exit status.
func TestRunPhases_InterruptedAfterTheLastPhase(t *testing.T) {
	t.Parallel()
	runDir := t.TempDir()
	a := &attempt{runDir: runDir, work: filepath.Join(runDir, "work"), stderr: io.Discard, rec: &record{Phases: []phaseRecord{}}}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	steps := []step{{name: "verify", run: func(context.Context) result {
		cancel(errors.New("interrupted by SIGINT"))
		return completed()
	}}}

	a.runPhases(ctx, steps)
	last := a.rec.Phases[len(a.rec.Phases)-1]
	if a.rec.Outcome != outcomeCompleted || last.Name != "cleanup" || last.Reason == nil {
		t.Errorf("outcome %s, last phase %+v; want completed with cleanup naming the signal", a.rec.Outcome, last)
	}
}

// TestRunPhases_RecordsRecordingFailures breaks the record after one phase:
// no later phase runs, and the record names the failure on a phase, so it
// explains the nonzero exit.
func TestRunPhases_RecordsRecordingFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		breakAfter string
		want       []string
	}{
		{"source", phaseNames("completed", "failed", "skipped", "skipped", "skipped", "skipped", "completed")},
		{"apply", phaseNames("completed", "completed", "completed", "completed", "completed", "uncertain", "completed")},
		{"verify", phaseNames("completed", "completed", "completed", "completed", "completed", "completed", "failed")},
	} {
		runDir := t.TempDir()
		a := &attempt{req: request{verb: verbApp}, runDir: runDir, work: filepath.Join(runDir, "work"), stderr: io.Discard, rec: &record{Phases: []phaseRecord{}}}
		var ran []string
		var steps []step
		for _, name := range []string{"source", "prepare", "inspect", "upload", "apply", "verify"} {
			steps = append(steps, step{name: name, run: func(context.Context) result {
				ran = append(ran, name)
				if name == tc.breakAfter {
					// A directory in its place makes every later write fail.
					record := filepath.Join(runDir, "record.json")
					os.Remove(record)
					os.Mkdir(record, 0o700)
				}
				return completed()
			}})
		}

		a.runPhases(context.Background(), steps)
		var got []string
		for _, p := range a.rec.Phases {
			got = append(got, p.Name+" "+p.Outcome)
		}
		want := outcomeFailed
		if tc.breakAfter == "apply" {
			want = outcomeUncertain // the deploy landed and is unverified
		}
		if !slices.Equal(got, tc.want) || a.rec.Outcome != want || !a.recordFailed {
			t.Errorf("breaking after %s: phases %q, outcome %s, recordFailed %v; want %q, %s, true",
				tc.breakAfter, got, a.rec.Outcome, a.recordFailed, tc.want, want)
		}
		if ran[len(ran)-1] != tc.breakAfter {
			t.Errorf("breaking after %s: ran %q", tc.breakAfter, ran)
		}
	}
}

// TestRedactor covers every form of a credential the logs must not show, and
// a secret straddling the size cap, which must not be written in part.
func TestRedactor(t *testing.T) {
	t.Parallel()
	const secret = "s3cr+t/=?& token"
	r := newRedactor(secret)
	var forms []string
	for _, v := range []string{secret, "x:" + secret} {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			forms = append(forms, enc.EncodeToString([]byte(v)))
		}
	}
	forms = append(forms, secret, url.QueryEscape(secret), url.PathEscape(secret))
	for _, f := range forms {
		if got := r.text("<" + f + ">"); got != "<[REDACTED]>" {
			t.Errorf("%q redacted to %q", f, got)
		}
	}

	// A capture cut where the secret runs past the cap.
	const limit = 20
	raw := strings.Repeat("a", limit-2) + secret + strings.Repeat("b", 100)
	data, cut := r.capped([]byte(raw[:limit+r.slack()]), true, limit)
	if !cut || len(data) > limit || strings.Contains(string(data), secret[:2]) {
		t.Errorf("capped to %q (cut %v): holds part of the secret", data, cut)
	}
	// A capture cut inside the secret.
	raw = strings.Repeat("a", limit+r.slack()-4) + secret
	if data, _ := r.capped([]byte(raw[:limit+r.slack()]), true, limit); strings.Contains(string(data), secret[:2]) {
		t.Errorf("capped to %q: holds part of the secret", data)
	}
}

// TestCapture keeps the first bytes of a stream and drains the rest, so a
// writer far past the cap and the pipe's buffer never blocks.
func TestCapture(t *testing.T) {
	t.Parallel()
	c, err := newCapture(10)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 1<<20)
	if _, err := c.w.WriteString(big); err != nil {
		t.Fatal(err)
	}
	if got := c.finish(); string(got) != big[:10] || c.total != int64(len(big)) {
		t.Errorf("kept %d bytes of %d, want 10 of %d", len(got), c.total, len(big))
	}
}
