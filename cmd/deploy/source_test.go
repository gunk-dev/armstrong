package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSource_UsesOnlyTheCommittedRevision(t *testing.T) {
	for _, form := range []string{"directory", "file URL"} {
		t.Run(form, func(t *testing.T) {
			h := newHarness(t)
			h.appFixture()
			want := h.rev
			// A later commit and uncommitted changes must not reach the build.
			h.commit(map[string]string{"marker": "later", "flake.lock": `{"later":true}`})
			h.write("infra/marker", "uncommitted")
			h.rev = want
			if form == "file URL" {
				h.infra = "file://" + h.infra
			}

			o := h.app()
			wantRun(t, o, 0, outcomeCompleted, phaseNames("completed", "completed", "completed", "completed", "completed", "completed", "completed")...)
			if *o.rec.ResolvedCommit != want || *o.rec.LockSHA256 != sha256Hex([]byte(baseFiles["flake.lock"])) || o.rec.Source != h.infra {
				t.Errorf("record names %s (lock %s) from %s", *o.rec.ResolvedCommit, *o.rec.LockSHA256, o.rec.Source)
			}
			for _, c := range h.calls() {
				if c.Tool == "nix" && (c.Info["marker"] != "committed" || c.Info["rev"] != want) {
					t.Errorf("nix built marker %q at %s, want the committed tree at %s", c.Info["marker"], c.Info["rev"], want)
				}
			}
		})
	}
}

func TestSource_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
	}{
		{"unknown commit", func(h *harness) {
			h.rev = strings.Repeat("1", 40)
		}},
		{"gitlink", func(h *harness) {
			h.gitIn(h.infra, "update-index", "--add", "--cacheinfo", "160000,"+h.rev+",vendor/sub")
			h.gitIn(h.infra, "commit", "--quiet", "-m", "submodule")
			h.rev = h.gitIn(h.infra, "rev-parse", "HEAD")
		}},
		{"flake.lock only in the working tree", func(h *harness) {
			h.rev = h.commit(map[string]string{"flake.lock": ""})
			h.write("infra/flake.lock", baseFiles["flake.lock"])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.appFixture()
			tc.setup(h)

			o := h.app()
			wantRun(t, o, 1, outcomeFailed, phaseNames("failed", "skipped", "skipped", "skipped", "skipped", "skipped", "completed")...)
			wantCleanedUp(t, o)
			for _, name := range h.callNames() {
				if !strings.HasPrefix(name, "git ") {
					t.Errorf("%s ran after the source was rejected", name)
				}
			}
		})
	}
}

func TestUsage(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	token := filepath.Join(h.root, "creds", "fly-token")
	key := filepath.Join(h.root, "creds", "porkbun-key")
	app := func(extra ...string) []string {
		return append([]string{"app", "demo", "--infra", h.infra, "--rev", h.rev, "--fly-token-file", token}, extra...)
	}
	dns := []string{"dns-sync", "--infra", h.infra, "--rev", h.rev, "--porkbun-api-key-file", key, "--porkbun-secret-key-file", key}
	for _, tc := range []struct {
		name     string
		args     []string
		stateDir string // overrides DEPLOY_STATE_DIR; "-" unsets it
	}{
		{name: "no command"},
		{name: "unknown command", args: []string{"status"}},
		{name: "app without a name", args: []string{"app", "--infra", h.infra, "--rev", h.rev, "--fly-token-file", token}},
		{name: "app with two names", args: append(app(), "other")},
		{name: "invalid app name", args: []string{"app", "Demo", "--infra", h.infra, "--rev", h.rev, "--fly-token-file", token}},
		{name: "no --infra", args: []string{"app", "demo", "--rev", h.rev, "--fly-token-file", token}},
		{name: "no --rev", args: []string{"app", "demo", "--infra", h.infra, "--fly-token-file", token}},
		{name: "short --rev", args: []string{"app", "demo", "--infra", h.infra, "--rev", h.rev[:12], "--fly-token-file", token}},
		{name: "uppercase --rev", args: []string{"app", "demo", "--infra", h.infra, "--rev", strings.ToUpper(h.rev), "--fly-token-file", token}},
		{name: "no --fly-token-file", args: []string{"app", "demo", "--infra", h.infra, "--rev", h.rev}},
		{name: "app dry run", args: app("--dry-run")},
		{name: "app target override", args: app("--app", "other-app")},
		{name: "app org override", args: app("--org", "other-org")},
		{name: "app environment override", args: app("--environment", "staging")},
		{name: "app image override", args: app("--image", "registry.fly.io/other:latest")},
		{name: "credentials in the URL", args: []string{"app", "demo", "--infra", "https://user:TOPSECRET@example.invalid/infra.git", "--rev", h.rev, "--fly-token-file", token}},
		{name: "credentials in a malformed URL", args: []string{"app", "demo", "--infra", "https://user:TOPSECRET@exa mple.invalid/infra.git", "--rev", h.rev, "--fly-token-file", token}},
		{name: "credentials in an SSH remote", args: []string{"app", "demo", "--infra", "user:TOPSECRET@example.invalid:infra.git", "--rev", h.rev, "--fly-token-file", token}},
		{name: "plain HTTP", args: []string{"app", "demo", "--infra", "http://example.invalid/infra.git", "--rev", h.rev, "--fly-token-file", token}},
		{name: "SSH remote", args: []string{"app", "demo", "--infra", "git@example.invalid:infra.git", "--rev", h.rev, "--fly-token-file", token}},
		{name: "dns-sync argument", args: append(dns, "gunk.dev")},
		{name: "dns-sync without a secret key", args: dns[:len(dns)-2]},
		{name: "dns-sync with a Fly token", args: append(dns, "--fly-token-file", token)},
		{name: "relative state directory", args: app(), stateDir: "state"},
		{name: "unset state directory", args: app(), stateDir: "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := h.command(tc.args...)
			switch tc.stateDir {
			case "":
			case "-":
				cmd.Env = cmd.Env[1:]
			default:
				cmd.Env[0] = "DEPLOY_STATE_DIR=" + tc.stateDir
			}
			out, err := cmd.CombinedOutput()
			if code := cmd.ProcessState.ExitCode(); code != exitUsage {
				t.Errorf("exit status %d (%v), want %d\n%s", code, err, exitUsage, out)
			}
			if _, err := os.Stat(h.state); !os.IsNotExist(err) {
				t.Errorf("a usage error touched the state directory")
			}
			if strings.Contains(string(out), "TOPSECRET") {
				t.Errorf("a usage error printed a credential:\n%s", out)
			}
		})
	}

	for _, args := range [][]string{{"--help"}, {"app", "--help"}, {"dns-sync", "--help"}, {"--version"}} {
		out, err := h.command(args...).Output()
		if err != nil {
			t.Errorf("deploy %v: %v", args, err)
		}
		if args[0] == "--version" && !strings.Contains(string(out), "test-version") {
			t.Errorf("--version printed %q", out)
		}
	}
}

func TestCredentialFileMustNotBeEmpty(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	h.write("creds/fly-token", "\n")

	o := h.app()
	if o.code != exitFailure || o.rec != nil || len(h.calls()) != 0 {
		t.Errorf("exit %d, record %v, %d calls; want 1, none, none\n%s", o.code, o.rec != nil, len(h.calls()), o.stderr)
	}
}
