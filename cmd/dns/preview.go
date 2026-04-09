package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

const defaultDomain = "gunk.dev"

func newPreviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Manage preview DNS records",
	}

	cmd.AddCommand(newPreviewCreateCmd())
	cmd.AddCommand(newPreviewDeleteCmd())
	return cmd
}

func newPreviewCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <app> <pr-number>",
		Short: "Create a preview CNAME record",
		Long:  "Creates preview-{pr}.{app}.gunk.dev -> {app}-preview-{pr}.fly.dev",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			pr := args[1]
			// Validate PR number
			if _, err := strconv.Atoi(pr); err != nil {
				return fmt.Errorf("invalid PR number: %s", pr)
			}

			client, err := newPorkbunClient()
			if err != nil {
				return err
			}

			subdomain := fmt.Sprintf("preview-%s.%s", pr, app)
			target := fmt.Sprintf("%s-preview-%s.fly.dev", app, pr)

			fmt.Printf("CREATE CNAME %s.%s -> %s\n", subdomain, defaultDomain, target)
			return client.create(defaultDomain, createRequest{
				Type:    "CNAME",
				Name:    subdomain,
				Content: target,
				TTL:     "600",
			})
		},
	}
}

func newPreviewDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <app> <pr-number>",
		Short: "Delete a preview CNAME record",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			pr := args[1]
			if _, err := strconv.Atoi(pr); err != nil {
				return fmt.Errorf("invalid PR number: %s", pr)
			}

			client, err := newPorkbunClient()
			if err != nil {
				return err
			}

			subdomain := fmt.Sprintf("preview-%s.%s", pr, app)

			fmt.Printf("DELETE CNAME %s.%s\n", subdomain, defaultDomain)
			return client.deleteByNameType(defaultDomain, "CNAME", subdomain)
		},
	}
}
