package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// gitEnv keeps git away from every configuration file and credential helper,
// never prompts, and allows only local and HTTPS transports.
var gitEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ALLOW_PROTOCOL=file:https",
}

func (a *attempt) git(ctx context.Context, label string, capture bool, args ...string) ([]byte, error) {
	return a.r.run(ctx, childSpec{
		label: label, tool: "git", args: args, dir: a.repo, env: gitEnv, capture: capture,
	})
}

// source fetches the requested commit into a fresh private repository and
// checks it out detached, so nothing but committed content at that commit is
// ever built or exported.
func (a *attempt) source(ctx context.Context) result {
	if err := a.makeWorkDirs(); err != nil {
		return failed("create work directory: %v", err)
	}
	if _, err := a.git(ctx, "git init", false, "init", "--quiet", "--template="); err != nil {
		return childResult(err, false)
	}
	if _, err := a.git(ctx, "git fetch", false, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", a.req.infra, a.req.rev); err != nil {
		return childResult(err, false)
	}
	out, err := a.git(ctx, "git rev-parse", true, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}")
	if err != nil {
		return childResult(err, false)
	}
	resolved := strings.TrimSpace(string(out))
	a.rec.ResolvedCommit = strPtr(resolved)
	if resolved != a.req.rev {
		return failed("fetched commit %s, want %s", resolved, a.req.rev)
	}
	if _, err := a.git(ctx, "git checkout", false, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", a.req.rev); err != nil {
		return childResult(err, false)
	}

	tree, err := a.git(ctx, "git ls-tree", true, "ls-tree", "-r", "-z", "--full-tree", a.req.rev)
	if err != nil {
		return childResult(err, false)
	}
	lockCommitted := false
	for _, entry := range bytes.Split(tree, []byte{0}) {
		// <mode> SP <type> SP <object> TAB <path>
		meta, path, ok := strings.Cut(string(entry), "\t")
		if !ok {
			continue
		}
		mode, _, _ := strings.Cut(meta, " ")
		if mode == "160000" {
			return failed("commit contains a gitlink (submodule) at %s", path)
		}
		if path == "flake.lock" && (mode == "100644" || mode == "100755") {
			lockCommitted = true
		}
	}
	if !lockCommitted {
		return failed("commit has no committed flake.lock")
	}
	lock, err := a.git(ctx, "git cat-file", true, "cat-file", "blob", a.req.rev+":flake.lock")
	if err != nil {
		return childResult(err, false)
	}
	sum := sha256Hex(lock)
	a.rec.LockSHA256 = &sum
	return completed()
}

// checkLock fails if the checkout's flake.lock differs from the committed
// one, as it would if a build rewrote it.
func (a *attempt) checkLock() error {
	data, err := os.ReadFile(filepath.Join(a.repo, "flake.lock"))
	if err != nil {
		return fmt.Errorf("read flake.lock: %w", err)
	}
	if sha256Hex(data) != *a.rec.LockSHA256 {
		return fmt.Errorf("flake.lock changed during the build")
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
