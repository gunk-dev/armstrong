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

func runGit(ctx context.Context, r *runner, repo, label string, capture bool, args ...string) ([]byte, error) {
	return r.run(ctx, childSpec{
		label: label, tool: "git", args: args, dir: repo, env: gitEnv, capture: capture,
	})
}

// checkoutCommit fetches rev from infra into the empty directory repo, verifies
// the resolved commit equals rev, checks it out detached, refuses gitlinks, and
// returns the resolved commit and the SHA-256 of the committed flake.lock.
// When requireMainAncestor is true, it fetches infra's refs/heads/main and
// requires rev to be an ancestor of (or equal to) it.
func checkoutCommit(ctx context.Context, r *runner, repo, infra, rev string, requireMainAncestor bool) (resolved, lockSHA string, err error) {
	if _, err := runGit(ctx, r, repo, "git init", false, "init", "--quiet", "--template="); err != nil {
		return "", "", err
	}
	if requireMainAncestor {
		if _, err := runGit(ctx, r, repo, "git fetch", false, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", infra, "refs/heads/main:refs/remotes/origin/main"); err != nil {
			return "", "", err
		}
		out, err := runGit(ctx, r, repo, "git rev-parse", true, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
		if err != nil {
			if isInterrupted(err) {
				return "", "", err
			}
			return "", "", fmt.Errorf("commit %s is not an ancestor of refs/heads/main", rev)
		}
		resolved = strings.TrimSpace(string(out))
		if resolved != rev {
			return resolved, "", fmt.Errorf("resolved commit %s, want %s", resolved, rev)
		}
		if _, err := runGit(ctx, r, repo, "git merge-base", false, "merge-base", "--is-ancestor", rev, "refs/remotes/origin/main"); err != nil {
			if isInterrupted(err) {
				return resolved, "", err
			}
			return resolved, "", fmt.Errorf("commit %s is not an ancestor of refs/heads/main", rev)
		}
	} else {
		if _, err := runGit(ctx, r, repo, "git fetch", false, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", infra, rev); err != nil {
			return "", "", err
		}
		out, err := runGit(ctx, r, repo, "git rev-parse", true, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}")
		if err != nil {
			return "", "", err
		}
		resolved = strings.TrimSpace(string(out))
		if resolved != rev {
			return resolved, "", fmt.Errorf("fetched commit %s, want %s", resolved, rev)
		}
	}
	if _, err := runGit(ctx, r, repo, "git checkout", false, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", rev); err != nil {
		return resolved, "", err
	}

	tree, err := runGit(ctx, r, repo, "git ls-tree", true, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return resolved, "", err
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
			return resolved, "", fmt.Errorf("commit contains a gitlink (submodule) at %s", path)
		}
		if path == "flake.lock" && (mode == "100644" || mode == "100755") {
			lockCommitted = true
		}
	}
	if !lockCommitted {
		return resolved, "", fmt.Errorf("commit has no committed flake.lock")
	}
	lock, err := runGit(ctx, r, repo, "git cat-file", true, "cat-file", "blob", rev+":flake.lock")
	if err != nil {
		return resolved, "", err
	}
	return resolved, sha256Hex(lock), nil
}

// source fetches the requested commit into a fresh private repository and
// checks it out detached, so nothing but committed content at that commit is
// ever built or exported.
func (a *attempt) source(ctx context.Context) result {
	if err := a.makeWorkDirs(); err != nil {
		return failed("create work directory: %v", err)
	}
	resolved, lockSHA, err := checkoutCommit(ctx, a.r, a.repo, a.req.infra, a.req.rev, a.req.requireMainAncestor)
	if resolved != "" {
		a.rec.ResolvedCommit = strPtr(resolved)
	}
	if err != nil {
		return childResult(err, false)
	}
	a.rec.LockSHA256 = &lockSHA
	return completed()
}

// checkLockFile fails if repo's flake.lock differs from wantSHA, as it would if
// a Nix command rewrote it.
func checkLockFile(repo, wantSHA string) error {
	data, err := os.ReadFile(filepath.Join(repo, "flake.lock"))
	if err != nil {
		return fmt.Errorf("read flake.lock: %w", err)
	}
	if sha256Hex(data) != wantSHA {
		return fmt.Errorf("flake.lock changed during the build")
	}
	return nil
}

// checkLock fails if the checkout's flake.lock differs from the committed
// one, as it would if a build rewrote it.
func (a *attempt) checkLock() error {
	return checkLockFile(a.repo, *a.rec.LockSHA256)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
