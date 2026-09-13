package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// loadWorkflow loads this repository's configuration with one workflow
// selected: the default, or a named workflow from `workflows:`.
func loadWorkflow(ctx context.Context, g *globals, name string) (workflow.Config, error) {
	config, err := workflow.Load(workflowRoot(ctx, g.dir))
	if err != nil {
		return config, err
	}
	return config.SelectWorkflow(name)
}

type workflowSummary struct {
	Name     string   `json:"name"`
	Template string   `json:"template,omitempty"`
	Stages   []string `json:"stages"`
}

func summarizeWorkflows(config workflow.Config) ([]workflowSummary, error) {
	var out []workflowSummary
	for _, name := range config.WorkflowNames() {
		selected, err := config.SelectWorkflow(name)
		if err != nil {
			return nil, err
		}
		stages, err := selected.Workflow.Order()
		if err != nil {
			return nil, err
		}
		out = append(out, workflowSummary{Name: name, Template: selected.Workflow.Template, Stages: stages})
	}
	return out, nil
}

func printWorkflows(out io.Writer, asJSON bool, config workflow.Config) error {
	summaries, err := summarizeWorkflows(config)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(out).Encode(summaries)
	}
	for _, s := range summaries {
		template := ""
		if s.Template != "" {
			template = " (" + s.Template + ")"
		}
		if _, err := fmt.Fprintf(out, "%s%s\n  %s\n", s.Name, template, strings.Join(s.Stages, " → ")); err != nil {
			return err
		}
	}
	return nil
}
