package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPinnedTools runs the real skopeo and flyctl, when they are on PATH, with
// the arguments and configuration deploy builds, so a flag or archive the
// pinned versions reject fails here rather than only against the fakes. It
// needs no network: skopeo copies the archive to a local directory, and
// flyctl only prints help.
func TestPinnedTools(t *testing.T) {
	t.Parallel()
	skopeo, err1 := exec.LookPath("skopeo")
	flyctl, err2 := exec.LookPath("flyctl")
	if err1 != nil || err2 != nil {
		t.Skip("skopeo or flyctl not on PATH")
	}
	root := t.TempDir()
	a := &attempt{work: filepath.Join(root, "work"), secrets: secrets{flyToken: flyToken}}
	t.Cleanup(func() {
		if a.auth != nil {
			a.auth.Close()
		}
	})
	r, err := os.CreateTemp(root, "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a.r = &runner{
		tools: map[string]string{"skopeo": skopeo, "flyctl": flyctl},
		path:  filepath.Dir(skopeo) + ":" + filepath.Dir(flyctl),
		home:  filepath.Join(a.work, "home"), runtime: filepath.Join(a.work, "run"), tmp: filepath.Join(a.work, "tmp"),
		lock:  r,
		grace: killGrace,
	}
	for _, d := range []string{a.work, a.r.home, a.r.runtime, a.r.tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.writeContainerConfig(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// skopeo unpacks the archive and preserves the manifest digest deploy
	// computed, both when copying and when reading the manifest raw.
	img := filepath.Join(root, "image.tar")
	writeImage(t, img, image{arch: "amd64"})
	info, err := inspectArchive(img)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(root, "copied")
	if _, err := a.skopeo(ctx, "skopeo copy", false, false, "copy", "--preserve-digests", "oci-archive:"+img, "dir:"+copied); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(copied, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.skopeo(ctx, "skopeo inspect", false, true, "inspect", "--raw", "oci-archive:"+img)
	if err != nil {
		t.Fatal(err)
	}
	for what, data := range map[string][]byte{"copied": manifest, "raw": raw} {
		if got := "sha256:" + sha256Hex(data); got != info.digest {
			t.Errorf("skopeo %s manifest %s, want %s", what, got, info.digest)
		}
	}

	// skopeo reads the registry auth through the inherited descriptor of the
	// unlinked auth file, as a mutator and as a reader: login --get-login
	// prints the user it finds there, offline.
	for _, mutator := range []bool{true, false} {
		login, err := a.skopeo(ctx, "skopeo login", mutator, true, "login", "--get-login", registry)
		if err != nil || strings.TrimSpace(string(login)) != "x" {
			t.Errorf("skopeo found user %q in the auth descriptor (mutator %v): %v", login, mutator, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(a.work, "containers")); len(entries) != 3 {
		t.Errorf("containers holds %v, want only policy.json, registries.conf and registries.d", entries)
	}

	// flyctl accepts every flag deploy passes it.
	for _, args := range [][]string{flyListArgs("machines", testApp), flyListArgs("volumes", testApp), flyDeployArgs("fly.json", testApp, "image")} {
		var command, flags []string
		for _, arg := range args {
			switch {
			case strings.HasPrefix(arg, "--"):
				flags = append(flags, arg)
			case len(flags) == 0:
				command = append(command, arg)
			}
		}
		help, err := a.r.run(ctx, childSpec{
			label: "flyctl help", tool: "flyctl", args: append(command, "--help"),
			dir: a.work, env: a.flyEnv(), capture: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range flags {
			if !regexp.MustCompile(`\s` + regexp.QuoteMeta(flag) + `\s`).Match(help) {
				t.Errorf("flyctl %s has no %s flag", strings.Join(command, " "), flag)
			}
		}
	}
}
