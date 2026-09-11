package main

import (
	"encoding/json"
	"fmt"

	"github.com/sam-bretz/envctl/internal/review"
	"github.com/spf13/cobra"
)

func runDiffCmd(g *globals) *cobra.Command {
	var req review.Request
	c := &cobra.Command{Use: "diff <run>", Short: "Compare retained checkpoint source, artifacts and review evidence", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		comparison, err := client.Diff(cmd.Context(), args[0], req)
		if err != nil {
			return err
		}
		if g.jsonOut {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(comparison)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), review.Text(comparison))
		return err
	}}
	c.Flags().StringVar(&req.Node, "node", "", "target checkpoint stage")
	_ = c.MarkFlagRequired("node")
	c.Flags().StringVar(&req.Revision, "revision", "", "target revision (defaults to current)")
	c.Flags().StringVar(&req.From, "from", "", "comparison checkpoint ID (defaults to target revision source pins)")
	return c
}
