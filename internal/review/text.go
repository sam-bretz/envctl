package review

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func Text(c Comparison) string {
	label := func(e Endpoint) string {
		if e.Checkpoint == nil {
			return e.Revision + " source pins"
		}
		s := e.Revision + " / " + e.Checkpoint.Node + " / " + e.Checkpoint.ID
		if e.Checkpoint.HistoricalOnly {
			s += " (historical only)"
		}
		return s
	}
	lines := []string{"From: " + label(c.From), "To:   " + label(c.To), "", "Objective before: " + c.From.Objective, "Objective after:  " + c.To.Objective, "Configuration: " + c.From.ConfigDigest + " -> " + c.To.ConfigDigest}
	for _, side := range []struct {
		name string
		e    Endpoint
	}{{"Before", c.From}, {"After", c.To}} {
		if cp := side.e.Checkpoint; cp != nil {
			lines = append(lines, "", side.name+" checkpoint: "+cp.Result.Summary, "Supervisor: "+cp.Result.Review.Summary)
			if cp.Approval != nil {
				lines = append(lines, "Approved by "+cp.Approval.Actor+" · "+cp.Approval.ResultDigest)
			} else {
				lines = append(lines, "No approval recorded")
			}
			for _, check := range cp.Result.Checks {
				lines = append(lines, fmt.Sprintf("Check %s: passed=%t · evidence %s", check.Name, check.Passed, check.EvidenceDigest))
			}
			for _, requirement := range cp.Result.Requirements {
				lines = append(lines, fmt.Sprintf("Requirement %s -> %s: %s", requirement.Capability, strings.Join(requirement.Nodes, ", "), requirement.Reason))
			}
			mergeLines := []string{}
			for repo, parents := range cp.Result.MergeParents {
				for parent, pin := range parents {
					mergeLines = append(mergeLines, "Merge input "+repo+" from "+parent+": "+pin)
				}
			}
			sort.Strings(mergeLines)
			lines = append(lines, mergeLines...)
			ids := make([]string, 0, len(cp.Result.SourceObjects))
			for id := range cp.Result.SourceObjects {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				lines = append(lines, "Submodule/LFS objects "+id+": "+cp.Result.SourceObjects[id].Digest)
			}
		}
	}
	artifacts := func(e Endpoint) map[string]string {
		m := map[string]string{}
		if e.Checkpoint != nil {
			for _, a := range e.Checkpoint.Result.Artifacts {
				m[a.Name] = a.Digest
			}
		}
		return m
	}
	datasets := func(e Endpoint) map[string]string {
		m := map[string]string{}
		if e.Checkpoint != nil {
			for id, d := range e.Checkpoint.Result.Datasets {
				m[id] = d.Source.Digest
			}
		}
		return m
	}
	changes := func(title string, a, b map[string]string) {
		ids := map[string]bool{}
		for id := range a {
			ids[id] = true
		}
		for id := range b {
			ids[id] = true
		}
		keys := make([]string, 0, len(ids))
		for id := range ids {
			keys = append(keys, id)
		}
		sort.Strings(keys)
		lines = append(lines, "", title)
		for _, id := range keys {
			if a[id] == b[id] {
				lines = append(lines, id+": unchanged · "+a[id])
			} else {
				lines = append(lines, id+": "+display(a[id])+" -> "+display(b[id]))
			}
		}
	}
	changes("Artifacts", artifacts(c.From), artifacts(c.To))
	changes("Dataset snapshots", datasets(c.From), datasets(c.To))
	for _, r := range c.Repositories {
		lines = append(lines, "", "Repository "+r.Repository+": "+display(r.Before)+" -> "+display(r.After))
		switch {
		case r.Unavailable != "":
			lines = append(lines, "Diff unavailable: "+r.Unavailable)
		case r.Before == r.After:
			lines = append(lines, "No source changes")
		default:
			lines = append(lines, r.Patch)
		}
		if r.Truncated {
			lines = append(lines, "[Diff preview truncated at 1 MiB; full source bundles remain retained.]")
		}
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, ansi.Strip(strings.Join(lines, "\n")))
}
func display(s string) string {
	if s == "" {
		return "(absent)"
	}
	return s
}
