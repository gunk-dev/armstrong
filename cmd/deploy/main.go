// Command deploy runs deployments from an exact revision of an infra
// repository: `deploy app <name>` builds, uploads and deploys one Fly app,
// `deploy dns-sync` converges the repository's DNS zones on Porkbun, and
// `deploy converge` runs dns-sync followed by every app the flake exposes,
// skipping apps that are already at that commit's outputs.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// Stamped by flake.nix with -ldflags -X.
var (
	// version is the runner's own version: printed by --version and recorded
	// as runner_version.
	version = "dev"
	// nixSystem is the Nix system the binary was packaged for. It selects the
	// infra outputs to build and the only image platform accepted. Empty means
	// the Linux system matching GOARCH.
	nixSystem = ""
	// toolPath is the colon-separated PATH, fixed at packaging time, that holds
	// git, nix, cue, flyctl, skopeo and dns. Tools are looked up only here, and
	// every child runs with exactly this PATH.
	toolPath = ""
	// storeDir is the Nix store every build output must resolve into.
	storeDir = "/nix/store"
	// verifyTimeout and verifyInterval bound post-deploy verification. They are
	// link-time settings so tests can shorten them.
	verifyTimeout  = "60s"
	verifyInterval = "2s"
)

const (
	exitOK         = 0
	exitFailure    = 1
	exitUsage      = 2
	exitContention = 75
	exitSIGINT     = 130
	exitSIGTERM    = 143
	exitSIGHUP     = 129
)

const (
	verbApp      = "app"
	verbDNS      = "dns-sync"
	verbConverge = "converge"
)

const maxCommitFileBytes = 1 << 20

var (
	revRE     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	nameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// request is a validated invocation.
type request struct {
	verb                string
	name                string // app only
	dryRun              bool   // dns-sync and converge
	infra               string // absolute directory or credential-free https/file URL
	rev                 string
	requireMainAncestor bool // true when rev came from --commit-file
	stateDir            string

	flyTokenFile      string
	porkbunKeyFile    string
	porkbunSecretFile string
}

// usageError marks a mistake in the invocation itself; it exits 2.
type usageError struct{ error }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var req *request
	root := newRootCmd(&req)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	// The commands only parse and validate, so every error Execute returns is
	// a usage error.
	if err := root.Execute(); err != nil {
		fmt.Fprintf(stderr, "deploy: %v\nRun 'deploy --help' for usage.\n", err)
		return exitUsage
	}
	if req == nil {
		return exitOK // --help or --version
	}
	return execute(*req, stdout, stderr)
}

const rootLong = `deploy runs deployments from an exact commit of an infra repository and records
what it did.

  deploy app <name>      build, upload and deploy one Fly app
  deploy dns-sync        converge the repository's DNS zones on Porkbun
  deploy converge        run dns-sync, then deploy each app that differs

Every command requires:
  --infra <checkout|git-url>  a repository directory, or an https:// or file://
                              Git URL without embedded credentials
  --rev <commit>              the full 40-character lowercase commit to deploy
                              (converge also accepts --commit-file <file>)
  DEPLOY_STATE_DIR            an absolute directory for deploy.lock, per-app
                              pointers under apps/ and per-attempt records
                              under runs/<id>/record.json, owned by this user
                              and writable by no one else

Only committed content at --rev is used; the working tree of a checkout is
ignored. One invocation runs at a time per state directory. Each attempt prints
the absolute path of its record first, moves through the phases source,
prepare, inspect, upload, apply, verify and cleanup, and keeps every child's
redacted output under runs/<id>/logs. An uncertain record means a change to
the target started and was confirmed neither done nor undone: inspect the
target before running again.

Exit status:
  0    completed, skipped, or a completed dry run
  2    usage error
  75   another attempt holds the lock
  129  interrupted by SIGHUP
  130  interrupted by SIGINT
  143  interrupted by SIGTERM
  1    anything else, including partial completion`

func newRootCmd(out **request) *cobra.Command {
	root := &cobra.Command{
		Use:           "deploy",
		Short:         "Deploy Fly apps and sync DNS from an exact infra revision",
		Long:          rootLong,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q", args[0])
			}
			return nil
		},
		RunE: func(*cobra.Command, []string) error {
			return errors.New("a command is required: app, dns-sync or converge")
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetVersionTemplate("deploy {{.Version}}\n")
	root.AddCommand(newAppCmd(out), newDNSSyncCmd(out), newConvergeCmd(out))
	return root
}

// commonFlags are the flags every command takes.
type commonFlags struct {
	infra string
	rev   string
}

func (c *commonFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&c.infra, "infra", "", "infra repository: a directory, or an https:// or file:// Git URL (required)")
	cmd.Flags().StringVar(&c.rev, "rev", "", "full 40-character lowercase commit to deploy (required)")
}

func (c *commonFlags) requestBase(verb string) (*request, error) {
	if c.infra == "" {
		return nil, usagef("--infra is required")
	}
	infra, err := parseInfra(c.infra)
	if err != nil {
		return nil, err
	}
	stateDir := os.Getenv("DEPLOY_STATE_DIR")
	if stateDir == "" {
		return nil, usagef("DEPLOY_STATE_DIR is not set")
	}
	if !filepath.IsAbs(stateDir) {
		return nil, usagef("DEPLOY_STATE_DIR %q is not an absolute path", stateDir)
	}
	return &request{verb: verb, infra: infra, stateDir: filepath.Clean(stateDir)}, nil
}

func (c *commonFlags) request(verb string) (*request, error) {
	req, err := c.requestBase(verb)
	if err != nil {
		return nil, err
	}
	if c.rev == "" {
		return nil, usagef("--rev is required")
	}
	if !revRE.MatchString(c.rev) {
		return nil, usagef("--rev %q is not a full 40-character lowercase commit", c.rev)
	}
	req.rev = c.rev
	return req, nil
}

// readCommitFile reads path once as data, accepts only NAME=value lines,
// ignores every name except VALLEY_COMMIT, and returns the 40-character
// lowercase commit or a usage error.
func readCommitFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", usagef("--commit-file: %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCommitFileBytes+1))
	if err != nil {
		return "", usagef("--commit-file: %v", err)
	}
	if len(data) > maxCommitFileBytes {
		return "", usagef("--commit-file %s exceeds %d bytes", path, maxCommitFileBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", usagef("--commit-file %s contains a NUL byte", path)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return "", usagef("--commit-file %s has no VALLEY_COMMIT", path)
	}
	var commit string
	seen := false
	for i, line := range strings.Split(text, "\n") {
		name, val, ok := strings.Cut(line, "=")
		if !ok || !envNameRE.MatchString(name) {
			return "", usagef("--commit-file %s: line %d is not of the form NAME=value", path, i+1)
		}
		if name != "VALLEY_COMMIT" {
			continue
		}
		if seen {
			return "", usagef("--commit-file %s: duplicate VALLEY_COMMIT", path)
		}
		seen = true
		if !revRE.MatchString(val) {
			return "", usagef("--commit-file %s: VALLEY_COMMIT %q is not a full 40-character lowercase commit", path, val)
		}
		commit = val
	}
	if !seen {
		return "", usagef("--commit-file %s has no VALLEY_COMMIT", path)
	}
	return commit, nil
}

// parseInfra accepts a repository directory, made absolute, or an
// unauthenticated https:// or file:// URL.
func parseInfra(s string) (string, error) {
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			// The error quotes the URL, which may hold credentials.
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err
			}
			return "", usagef("--infra is not a valid URL: %v", err)
		}
		if u.User != nil {
			return "", usagef("--infra must not embed credentials")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return "", usagef("--infra must not carry a query or fragment")
		}
		switch u.Scheme {
		case "https":
			if u.Host == "" {
				return "", usagef("--infra has no host")
			}
		case "file":
			if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
				return "", usagef("--infra is not a file:///absolute/path URL")
			}
		default:
			return "", usagef("--infra scheme %q is not supported; use https:// or file://", u.Scheme)
		}
		return s, nil
	}
	// A colon before the first slash is scp-like SSH syntax (host:path).
	if i := strings.IndexByte(s, ':'); i >= 0 {
		if j := strings.IndexByte(s, '/'); j < 0 || i < j {
			return "", usagef("--infra looks like an SSH remote; use a directory or an https:// or file:// URL")
		}
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		return "", usagef("--infra: %v", err)
	}
	return abs, nil
}

func exactArgs(n int, what string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return fmt.Errorf("%s, got %d arguments", what, len(args))
		}
		return nil
	}
}

func newAppCmd(out **request) *cobra.Command {
	var common commonFlags
	var tokenFile string
	cmd := &cobra.Command{
		Use:   "app <name> --infra <checkout|git-url> --rev <commit> --fly-token-file <file>",
		Short: "Build, upload and deploy one Fly app",
		Long: `Builds the infra outputs for <name> at --rev, pushes the image to the Fly
registry and runs an ordinary fly deploy of it by digest.

The infra flake provides, for the packaged Nix system:
  packages.<system>.deploy-<name>-image   one single-platform OCI archive whose
                                          derivation depends on the image's
                                          smoke check
  packages.<system>.deploy-<name>-config  a directory holding only fly.json; its
                                          "app" names the Fly app to deploy

The Fly app and its Machines must already exist. The image is pushed as
registry.fly.io/<app>:<attempt-id>, its manifest digest is checked against the
local build, and fly deploy runs with --image registry.fly.io/<app>@<digest>.
Verification then requires the same volumes and Machine count as before,
every started, stopped or suspended Machine on the digest, and a started
Machine unless fly.json lets every Machine stop.

--fly-token-file names a file holding the Fly API token; only flyctl and skopeo
see it.`,
		Args: exactArgs(1, "app takes exactly one name"),
		RunE: func(_ *cobra.Command, args []string) error {
			if !nameRE.MatchString(args[0]) {
				return usagef("app name %q must be lowercase letters, digits and dashes", args[0])
			}
			req, err := common.request(verbApp)
			if err != nil {
				return err
			}
			if tokenFile == "" {
				return usagef("--fly-token-file is required")
			}
			req.name = args[0]
			req.flyTokenFile = tokenFile
			*out = req
			return nil
		},
	}
	common.register(cmd)
	cmd.Flags().StringVar(&tokenFile, "fly-token-file", "", "file holding the Fly API token (required)")
	return cmd
}

func newDNSSyncCmd(out **request) *cobra.Command {
	var common commonFlags
	var keyFile, secretFile string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "dns-sync --infra <checkout|git-url> --rev <commit> --porkbun-api-key-file <file> --porkbun-secret-key-file <file> [--dry-run]",
		Short: "Converge the infra repository's DNS zones on Porkbun",
		Long: `Vets and exports the CUE zones under dns/ at --rev and runs
dns sync --prune once over all of them.

Each immediate directory of dns/ is one zone package, taken in sorted order; with
no directories, dns/ itself is one zone. Every zone is vetted before any is
exported. dns sync never prunes NS, SOA or preview-* records.

--dry-run prints the plan, with both keys redacted, and changes nothing.

The key files hold the Porkbun API key and secret key; only dns sees them.`,
		Args: exactArgs(0, "dns-sync takes no arguments"),
		RunE: func(*cobra.Command, []string) error {
			req, err := common.request(verbDNS)
			if err != nil {
				return err
			}
			if keyFile == "" || secretFile == "" {
				return usagef("--porkbun-api-key-file and --porkbun-secret-key-file are both required")
			}
			req.dryRun = dryRun
			req.porkbunKeyFile = keyFile
			req.porkbunSecretFile = secretFile
			*out = req
			return nil
		},
	}
	common.register(cmd)
	cmd.Flags().StringVar(&keyFile, "porkbun-api-key-file", "", "file holding the Porkbun API key (required)")
	cmd.Flags().StringVar(&secretFile, "porkbun-secret-key-file", "", "file holding the Porkbun secret key (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without changing any record")
	return cmd
}

func newConvergeCmd(out **request) *cobra.Command {
	var common commonFlags
	var commitFile, tokenFile, keyFile, secretFile string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "converge --infra <checkout|git-url> (--rev <commit> | --commit-file <file>) --fly-token-file <file> --porkbun-api-key-file <file> --porkbun-secret-key-file <file> [--dry-run]",
		Short: "Converge DNS and every app the infra flake exposes at a commit",
		Long: `Runs a sequence of ordinary attempts under one lock: dns-sync first, then each
app the infra flake exposes for the packaged Nix system, in sorted order.

The target commit comes from either --rev or --commit-file (mutually exclusive).
--commit-file reads a file of NAME=value lines once as data, takes VALLEY_COMMIT
(ignoring other names), and requires it to be an ancestor of (or equal to)
refs/heads/main in --infra before any attempt starts.

Apps are discovered from packages.<system>.deploy-<name>-image and
packages.<system>.deploy-<name>-config; each app's config output is built during
discovery to resolve its Fly app name. Unpaired outputs, invalid names, or two
outputs targeting the same Fly app are refused before any attempt starts.

An app is skipped only when the per-app pointer under DEPLOY_STATE_DIR/apps/
names a completed deploy with the same flake app, Fly app, platform, fly.json
SHA-256 and image digest, and live inspection confirms the Machines still settle
on that digest (with a started Machine when fly.json keeps one running). Any
doubt deploys. A skipped app is recorded with outcome "skipped" and a reason
stating what was compared. Standalone "deploy app <name>" never skips.

The first failed or uncertain attempt stops the sequence; remaining apps are
not attempted. A summary at the end lists every target, its outcome and record
path, and what was not attempted.

--dry-run runs dns-sync --dry-run and, per app, builds, inspects and reports
whether it would skip or deploy without changing anything.`,
		Args: exactArgs(0, "converge takes no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := common.requestBase(verbConverge)
			if err != nil {
				return err
			}
			revSet := cmd.Flags().Changed("rev")
			fileSet := cmd.Flags().Changed("commit-file")
			switch {
			case revSet && fileSet:
				return usagef("--rev and --commit-file are mutually exclusive")
			case fileSet:
				if commitFile == "" {
					return usagef("--commit-file must not be empty")
				}
				rev, err := readCommitFile(commitFile)
				if err != nil {
					return err
				}
				req.rev = rev
				req.requireMainAncestor = true
			case revSet:
				if common.rev == "" {
					return usagef("--rev is required")
				}
				if !revRE.MatchString(common.rev) {
					return usagef("--rev %q is not a full 40-character lowercase commit", common.rev)
				}
				req.rev = common.rev
			default:
				return usagef("--rev or --commit-file is required")
			}
			if tokenFile == "" {
				return usagef("--fly-token-file is required")
			}
			if keyFile == "" || secretFile == "" {
				return usagef("--porkbun-api-key-file and --porkbun-secret-key-file are both required")
			}
			req.dryRun = dryRun
			req.flyTokenFile = tokenFile
			req.porkbunKeyFile = keyFile
			req.porkbunSecretFile = secretFile
			*out = req
			return nil
		},
	}
	cmd.Flags().StringVar(&common.infra, "infra", "", "infra repository: a directory, or an https:// or file:// Git URL (required)")
	cmd.Flags().StringVar(&common.rev, "rev", "", "full 40-character lowercase commit to deploy (mutually exclusive with --commit-file)")
	cmd.Flags().StringVar(&commitFile, "commit-file", "", "file of NAME=value lines providing VALLEY_COMMIT, required to be on refs/heads/main (mutually exclusive with --rev)")
	cmd.Flags().StringVar(&tokenFile, "fly-token-file", "", "file holding the Fly API token (required)")
	cmd.Flags().StringVar(&keyFile, "porkbun-api-key-file", "", "file holding the Porkbun API key (required)")
	cmd.Flags().StringVar(&secretFile, "porkbun-secret-key-file", "", "file holding the Porkbun secret key (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "run dns-sync --dry-run and report per-app skip/deploy decisions without changing anything")
	return cmd
}
