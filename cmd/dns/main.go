package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "dns",
		Short: "Manage DNS records via the Porkbun API",
	}

	root.AddCommand(newSyncCmd())
	root.AddCommand(newPreviewCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.As(err, new(errNoWrite)) {
			os.Exit(exitNoWrite)
		}
		os.Exit(1)
	}
}
