package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sam-bretz/envctl/internal/tui"
	"github.com/sam-bretz/envctl/internal/workflow"
	"github.com/spf13/cobra"
)

func runPRCmd(g *globals) *cobra.Command {
	var open bool
	c := &cobra.Command{Use: "pr <run>", Short: "Print, or open, the pull requests a run published", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		run, err := client.Get(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return printPullRequests(cmd.OutOrStdout(), g.jsonOut, open, run, tui.OpenInBrowser)
	}}
	c.Flags().BoolVar(&open, "open", false, "open each pull request in the browser")
	return c
}

func printPullRequests(out io.Writer, asJSON, open bool, run *workflow.Run, opener func(string) error) error {
	prs := run.PullRequests()
	if asJSON {
		if prs == nil {
			prs = []workflow.PullRequest{}
		}
		if err := json.NewEncoder(out).Encode(prs); err != nil {
			return err
		}
	} else if len(prs) == 0 {
		return errors.New("no pull request yet; the approved change opens one after approval")
	}
	for _, pr := range prs {
		if !asJSON {
			if _, err := fmt.Fprintf(out, "%s  %s\n", pr.URL, pr.Key); err != nil {
				return err
			}
		}
		if open {
			if err := opener(pr.URL); err != nil {
				return err
			}
		}
	}
	return nil
}
