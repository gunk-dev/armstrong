package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	outcomeCompleted = "completed"
	outcomeFailed    = "failed"
	outcomeSkipped   = "skipped"
	outcomeUncertain = "uncertain"
)

// record is runs/<attempt-id>/record.json. Unavailable values are null.
type record struct {
	SchemaVersion   int               `json:"schema_version"`
	AttemptID       string            `json:"attempt_id"`
	Verb            string            `json:"verb"`
	Name            *string           `json:"name"`
	DryRun          bool              `json:"dry_run"`
	Source          string            `json:"source"`
	RequestedCommit string            `json:"requested_commit"`
	ResolvedCommit  *string           `json:"resolved_commit"`
	LockSHA256      *string           `json:"lock_sha256"`
	ConfigSHA256    map[string]string `json:"config_sha256"`
	Platform        string            `json:"platform"`
	RunnerVersion   string            `json:"runner_version"`
	ImageDigest     *string           `json:"image_digest"`
	TargetsBefore   *targets          `json:"targets_before"`
	TargetsAfter    *targets          `json:"targets_after"`
	StartedAt       string            `json:"started_at"`
	FinishedAt      *string           `json:"finished_at"`
	Outcome         string            `json:"outcome"`
	Phases          []phaseRecord     `json:"phases"`
	Logs            string            `json:"logs"`
}

type phaseRecord struct {
	Name    string  `json:"name"`
	Outcome string  `json:"outcome"`
	Reason  *string `json:"reason"`
}

// targets is a Fly app as flyctl reports it, with Machines and volumes sorted
// by ID.
type targets struct {
	App      string    `json:"app"`
	Machines []machine `json:"machines"`
	Volumes  []volume  `json:"volumes"`
}

type machine struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	ImageDigest *string `json:"image_digest"`
}

type volume struct {
	ID                string  `json:"id"`
	AttachedMachineID *string `json:"attached_machine_id"`
}

func timestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// setPhase adds the phase or replaces its entry.
func (r *record) setPhase(name, outcome, reason string) {
	p := phaseRecord{Name: name, Outcome: outcome, Reason: strPtr(reason)}
	for i := range r.Phases {
		if r.Phases[i].Name == name {
			r.Phases[i] = p
			return
		}
	}
	r.Phases = append(r.Phases, p)
}

// overall ranks the phase outcomes: any uncertain phase makes the attempt
// uncertain, then any failed phase makes it failed.
func overall(phases []phaseRecord) string {
	out := outcomeCompleted
	for _, p := range phases {
		switch p.Outcome {
		case outcomeUncertain:
			return outcomeUncertain
		case outcomeFailed:
			out = outcomeFailed
		}
	}
	return out
}

// writeAtomic replaces dir/name with data so that a crash leaves either the
// old or the new content: write and fsync a temporary file, rename it over the
// target, then fsync the directory.
func writeAtomic(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (r *record) marshal() ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
