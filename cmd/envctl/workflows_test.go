package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRunWorkflowsListsTheDefaultAndNamedWorkflows(t *testing.T) {
	config, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\nworkflows: {small: {template: small, nodes: {build: {checks: [{name: unit, command: [go, test, ./...]}]}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = printWorkflows(&out, false, config); err != nil {
		t.Fatal(err)
	}
	want := "default (feature)\n  task → plan → design → code → qa → approved-change\nsmall (small)\n  plan → build → approved-change\n"
	if out.String() != want {
		t.Fatalf("workflows output:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	if err = printWorkflows(&out, true, config); err != nil || !strings.Contains(out.String(), `"stages":["plan","build","approved-change"]`) {
		t.Fatalf("json output %s %v", out.String(), err)
	}
}
