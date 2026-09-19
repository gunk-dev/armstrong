package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// exitChangesPending is what `unifi diff` returns when the plan is non-empty,
// so CI can tell "changes are needed" apart from "the command failed".
const exitChangesPending = 2

func main() {
	err := newRootCmd().Execute()
	switch {
	case err == nil:
		return
	case errors.Is(err, errChangesPending):
		os.Exit(exitChangesPending)
	default:
		fmt.Fprintln(os.Stderr, redact(err.Error()))
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "unifi",
		Short: "Manage a UniFi Network site via the official Integration API",
		Long: "Reconciles a UniFi Network site against a JSON #Site document (pipe from:\n" +
			"cue export ./unifi --out json -e site).\n\n" +
			"Configuration comes from the environment:\n" +
			"  UNIFI_URL           console base URL, e.g. https://unifi.lan\n" +
			"  UNIFI_API_KEY       Integration API key (never printed)\n" +
			"  UNIFI_SITE          site name (default \"Default\")\n" +
			"  UNIFI_CA_FILE       PEM bundle for the console's self-signed certificate\n" +
			"  UNIFI_INSECURE_TLS  set to 1 to skip certificate verification instead\n\n" +
			"Objects are matched by name (DHCP reservations by MAC); SYSTEM_DEFINED objects are updated in place\n" +
			"but never deleted, even with --prune. DHCP reservations and the site-wide mDNS\n" +
			"proxy setting are read and written through the legacy controller API with the\n" +
			"same key, since the Integration API has neither. Passphrases live in the environment,\n" +
			"named by each SSID's passphraseEnv, and are redacted from all output.",
		SilenceUsage: true,
	}
	root.AddCommand(newExportCmd(), newDiffCmd(), newSyncCmd(), newRestoreCmd())
	return root
}

func newExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export",
		Short: "Print the live site as #Site-shaped JSON",
		Long: "Dumps networks, firewall zones, wifi, firewall policies, DNS policies, DHCP\n" +
			"reservations and the mDNS proxy setting as a\n" +
			"#Site-shaped JSON document, so a consumer repo can bootstrap its instance file\n" +
			"from real state. WiFi passphrases are never included.\n\n" +
			"Objects the schema cannot express faithfully — a firewall zone whose members are\n" +
			"WAN interfaces, a firewall policy using a field #FirewallPolicy does not model,\n" +
			"an mDNS proxy setting #MDNS cannot read with certainty —\n" +
			"are left out and named on stderr, so that feeding the output back into `diff`\n" +
			"stays a no-op instead of planning a lossy write.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, s, err := connect()
			if err != nil {
				return err
			}
			return exportSite(c, s, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func newDiffCmd() *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Print the plan without changing anything; exit 2 if changes are needed",
		Long: "Reads a #Site JSON document from stdin and prints what sync would do.\nExits 2 when any change would be made, so CI can gate on it.\n\n" +
			"With --prune, every deletion is marked as listed or NOT listed in the input's\n" +
			"`deletions`, and the plan ends with a note if sync would refuse to apply it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.dryRun = true
			changed, err := reconcile(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
			if err != nil {
				return err
			}
			if changed {
				cmd.SilenceErrors = true
				return errChangesPending
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.prune, "prune", false, "Include the deletions that sync --prune would make in the plan")
	cmd.Flags().IntVar(&opts.maxChanges, "max-changes", defaultMaxChanges, "Report when the plan deletes, updates or moves (firewall policy order) more objects than sync --max-changes would allow")
	return cmd
}

func newSyncCmd() *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Converge the site to the #Site JSON read from stdin",
		Long: "Reads a #Site JSON document from stdin (pipe from: cue export ./unifi --out json -e site)\n" +
			"and converges the UniFi site to match.\n\n" +
			"The whole plan is worked out before anything is written, and the run is refused —\n" +
			"with the plan printed and nothing changed — when:\n" +
			"  * --prune would delete an object whose key the input's `deletions` does not list, or\n" +
			"  * the plan deletes, updates or reorders more than --max-changes objects (every firewall\n" +
			"    policy whose position changes counts; creates do not).\n" +
			"--force overrides both. With --snapshot-dir, the live site is exported there before\n" +
			"the first write; `unifi restore` applies such a snapshot.\n\n" +
			"A declared mdns proxy service scope is reconciled before anything else, so it is\n" +
			"in force before a network in the same run starts to participate, and a failed\n" +
			"write to it aborts the run with nothing changed. It is only ever updated, never pruned.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := reconcile(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.prune, "prune", false, "Delete USER_DEFINED objects not declared in the input, and clear DHCP reservations it does not declare (the client record is kept), if the input's `deletions` lists them. SYSTEM_DEFINED objects are never deleted, and a resource type the input omits entirely is never pruned — but one it declares empty (e.g. \"dnsPolicies\": []) has every USER_DEFINED object of that type as a candidate")
	addWriteFlags(cmd, &opts)
	return cmd
}

func newRestoreCmd() *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   "restore <snapshot.json>",
		Short: "Converge the site back to a snapshot written by sync",
		Long: "Applies a snapshot written by `sync --snapshot-dir` (or any `unifi export` output) as\n" +
			"the desired state, through the same reconciler and guards as sync: objects are\n" +
			"matched by identity, never by id, and SYSTEM_DEFINED objects are never deleted.\n\n" +
			"A snapshot holds no passphrases. Each SSID names its passphraseEnv, as in an\n" +
			"instance file, and that variable must be set in the environment.\n\n" +
			"A snapshot lists no deletions, so undoing a create needs --prune --force.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = reconcile(f, cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.prune, "prune", false, "Delete USER_DEFINED objects the snapshot does not hold (needs --force, since a snapshot lists no deletions)")
	addWriteFlags(cmd, &opts)
	return cmd
}

// addWriteFlags registers the flags sync and restore share.
func addWriteFlags(cmd *cobra.Command, opts *options) {
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Print the planned changes without calling the API")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Apply the plan even if it deletes objects `deletions` does not list or exceeds --max-changes")
	cmd.Flags().IntVar(&opts.maxChanges, "max-changes", defaultMaxChanges, "Refuse a plan that deletes, updates or moves (firewall policy order) more than this many objects")
	cmd.Flags().StringVar(&opts.snapshotDir, "snapshot-dir", "", "Before the first write, save the live site there as a timestamped JSON export")
	cmd.Flags().IntVar(&opts.snapshotKeep, "snapshot-keep", 10, "Number of snapshots to keep in --snapshot-dir; 0 keeps all")
}

const defaultMaxChanges = 10

// options is how a command asked reconcile to run.
type options struct {
	prune, dryRun, force bool
	maxChanges           int
	snapshotDir          string
	snapshotKeep         int
}

// errChangesPending makes `unifi diff` exit with exitChangesPending when a
// change is planned. It is never printed: the plan itself is the output.
var errChangesPending = errors.New("changes required")

func connect() (*client, siteRef, error) {
	c, err := newClient()
	if err != nil {
		return nil, siteRef{}, err
	}
	s, err := c.site(siteName())
	if err != nil {
		return nil, siteRef{}, err
	}
	return c, s, nil
}

// reconcile converges the site to the #Site document read from in.
//
// A dry run is a single read-only pass. A real run is two: a planning pass
// that writes nothing and whose output is held back, then — only if no guard
// refuses the plan — a snapshot, and the pass that writes. That ordering is
// what makes a refusal fail closed: it happens before the first mutation, so
// a refused run changes nothing at all.
func reconcile(in io.Reader, out, errOut io.Writer, opts options) (bool, error) {
	data, err := io.ReadAll(in)
	if err != nil {
		return false, fmt.Errorf("read input: %w", err)
	}
	var want site
	if err := json.Unmarshal(data, &want); err != nil {
		return false, fmt.Errorf("parse input: %w", err)
	}

	c, s, err := connect()
	if err != nil {
		return false, err
	}
	newReconciler := func(dryRun bool, w io.Writer) *reconciler {
		return &reconciler{client: c, siteID: s.ID, legacySite: s.InternalReference, want: want, prune: opts.prune, force: opts.force, dryRun: dryRun, out: w}
	}

	if opts.dryRun {
		r := newReconciler(true, out)
		if err := r.run(); err != nil {
			return r.changed, err
		}
		if reasons := r.refusals(opts.maxChanges); len(reasons) > 0 {
			fmt.Fprintf(out, "\n*** sync would refuse this plan ***\n%s\n", formatRefusals(reasons))
		}
		return r.changed, nil
	}

	var planOut bytes.Buffer
	plan := newReconciler(true, &planOut)
	plan.planning = true
	if err := plan.run(); err != nil {
		_, _ = out.Write(planOut.Bytes())
		return plan.changed, err
	}
	if reasons := plan.refusals(opts.maxChanges); len(reasons) > 0 {
		_, _ = out.Write(planOut.Bytes())
		return plan.changed, fmt.Errorf("refusing to apply this plan; nothing was changed:\n%s", formatRefusals(reasons))
	}
	if plan.writes == 0 {
		// Nothing to write, so nothing to snapshot and no second pass: the
		// planning output is already the whole story.
		_, _ = out.Write(planOut.Bytes())
		return plan.changed, nil
	}

	if opts.snapshotDir != "" {
		path, err := writeSnapshot(c, s, want, opts.snapshotDir, opts.snapshotKeep, errOut)
		if err != nil {
			return false, err
		}
		fmt.Fprintf(out, "snapshot of the live site written to %s\n", path)
	}

	r := newReconciler(false, out)
	if err := r.run(); err != nil {
		return r.changed, err
	}
	return r.changed, nil
}

func formatRefusals(reasons []string) string {
	var b strings.Builder
	for i, reason := range reasons {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  - " + reason)
	}
	return b.String()
}
