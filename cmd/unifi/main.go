package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

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
			"  UNIFI_URL           console base URL, e.g. https://192.168.1.1\n" +
			"  UNIFI_API_KEY       Integration API key (never printed)\n" +
			"  UNIFI_SITE          site name (default \"Default\")\n" +
			"  UNIFI_CA_FILE       PEM bundle for the console's self-signed certificate\n" +
			"  UNIFI_INSECURE_TLS  set to 1 to skip certificate verification instead\n\n" +
			"Objects are matched by name; SYSTEM_DEFINED objects are updated in place\n" +
			"but never deleted, even with --prune.",
		SilenceUsage: true,
	}
	root.AddCommand(newExportCmd(), newDiffCmd(), newSyncCmd())
	return root
}

func newExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export",
		Short: "Print the live site as #Site-shaped JSON",
		Long:  "Dumps networks, firewall zones, wifi, firewall policies and DNS policies as a\n#Site-shaped JSON document, so a consumer repo can bootstrap its instance file\nfrom real state. WiFi passphrases are never included.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, siteID, err := connect()
			if err != nil {
				return err
			}
			return exportSite(c, siteID, cmd.OutOrStdout())
		},
	}
}

func newDiffCmd() *cobra.Command {
	var prune bool
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Print the plan without changing anything; exit 2 if changes are needed",
		Long:  "Reads a #Site JSON document from stdin and prints what sync would do.\nExits 2 when any change would be made, so CI can gate on it.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			changed, err := reconcile(cmd.InOrStdin(), cmd.OutOrStdout(), prune, true)
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
	cmd.Flags().BoolVar(&prune, "prune", false, "Include deletions of USER_DEFINED objects absent from the input")
	return cmd
}

func newSyncCmd() *cobra.Command {
	var prune, dryRun bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Converge the site to the #Site JSON read from stdin",
		Long:  "Reads a #Site JSON document from stdin (pipe from: cue export ./unifi --out json -e site)\nand converges the UniFi site to match.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := reconcile(cmd.InOrStdin(), cmd.OutOrStdout(), prune, dryRun)
			return err
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false, "Delete USER_DEFINED objects absent from the input (SYSTEM_DEFINED objects are never deleted)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the planned changes without calling the API")
	return cmd
}

// errChangesPending makes `unifi diff` exit with exitChangesPending when a
// change is planned. It is never printed: the plan itself is the output.
var errChangesPending = errors.New("changes required")

func connect() (*client, string, error) {
	c, err := newClient()
	if err != nil {
		return nil, "", err
	}
	id, err := c.siteID(siteName())
	if err != nil {
		return nil, "", err
	}
	return c, id, nil
}

func reconcile(in io.Reader, out io.Writer, prune, dryRun bool) (bool, error) {
	data, err := io.ReadAll(in)
	if err != nil {
		return false, fmt.Errorf("read stdin: %w", err)
	}
	var want site
	if err := json.Unmarshal(data, &want); err != nil {
		return false, fmt.Errorf("parse input: %w", err)
	}

	c, siteID, err := connect()
	if err != nil {
		return false, err
	}
	r := &reconciler{client: c, siteID: siteID, want: want, prune: prune, dryRun: dryRun, out: out}
	if err := r.run(); err != nil {
		return r.changed, err
	}
	return r.changed, nil
}
