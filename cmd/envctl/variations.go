package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func variationsCmd(g *globals) *cobra.Command {
	return &cobra.Command{Use: "variations <run>", Short: "Compare a run's variations side by side", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		run, err := client.Get(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		compared, err := review.CompareVariations(cmd.Context(), client, run)
		if err != nil {
			return err
		}
		if g.jsonOut {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(compared)
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), review.VariationsText(compared))
		return err
	}}
}

func chooseCmd(g *globals) *cobra.Command {
	var variation string
	c := &cobra.Command{Use: "choose <run>", Short: "Continue one variation to approval and retire the others", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		run, err := client.Get(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		target, err := run.FindVariation(variation)
		if err != nil {
			return err
		}
		result, err := client.Action(cmd.Context(), run.ID, daemon.ActionRequest{
			OperationID: workflow.ID("op"), ExpectedVersion: run.Version, Revision: target.ID, Action: "choose",
		})
		if err != nil {
			return err
		}
		return printRun(cmd, g, result)
	}}
	c.Flags().StringVar(&variation, "variation", "", "variation name or revision ID to continue")
	_ = c.MarkFlagRequired("variation")
	return c
}

// printVariationStatus says a comparison is running or waiting, and how to act
// on it, without printing the whole comparison into run show.
func printVariationStatus(out io.Writer, r *workflow.Run) {
	groups := r.VariationGroups()
	if len(groups) == 0 {
		return
	}
	summaries := r.CompareVariations(groups[len(groups)-1])
	fmt.Fprintln(out, "  variations:")
	waiting := false
	for _, s := range summaries {
		fmt.Fprintf(out, "    %-16s %s  %s\n", s.Name, s.Status, s.Revision)
		waiting = waiting || s.Status == workflow.VariationReady || s.Status == workflow.VariationBuilding
	}
	if waiting {
		fmt.Fprintf(out, "    compare with: envctl run variations %s\n    continue one: envctl run choose %s --variation <name>\n", r.ID, r.ID)
	}
}
