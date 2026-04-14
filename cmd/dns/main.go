package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "dns",
		Short: "Manage DNS records for gunk.dev via Porkbun API",
	}

	root.AddCommand(newSyncCmd())
	root.AddCommand(newPreviewCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
