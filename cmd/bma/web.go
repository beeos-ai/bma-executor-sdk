//go:build darwin || linux

package main

import (
	"github.com/spf13/cobra"
)

// webCmd is `bma search [query]` or `bma fetch [url]`: the generated command
// of the operation id (every body field a flag, --body, --idempotency-key)
// at the top level, with the operation's main field (query, url) also taken
// as the argument. Each run sends an Idempotency-Key (a new one unless
// --idempotency-key is given), so the client's retries are not charged twice.
func webCmd(id, use, field string) *cobra.Command {
	var op *apiOp
	for i := range apiOperations {
		if apiOperations[i].ID == id {
			o := apiOperations[i]
			o.Idempotent = true
			op = &o
		}
	}
	if op == nil {
		return nil
	}
	cmd := apiCommand(op)
	cmd.Use = use + " [" + field + "]"
	cmd.Args = cobra.MaximumNArgs(1)
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, a []string) error {
		if len(a) == 1 {
			if cmd.Flags().Changed(field) {
				return usageError("give the " + field + " as the argument or --" + field + ", not both")
			}
			if err := cmd.Flags().Set(field, a[0]); err != nil {
				return err
			}
		}
		return run(cmd, nil)
	}
	return cmd
}

func addWeb(root *cobra.Command) {
	for _, c := range []*cobra.Command{webCmd("createWebSearch", "search", "query"), webCmd("createWebFetch", "fetch", "url")} {
		if c != nil {
			root.AddCommand(c)
		}
	}
}
