package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/sam-bretz/envctl/internal/doctor"
	"github.com/spf13/cobra"
)

// errDoctorFailed is returned by doctorCmd's RunE when any check failed, so
// main() can exit 1 without printing anything past what doctorCmd already
// wrote (root() sets SilenceErrors, so cobra itself never prints either).
var errDoctorFailed = errors.New("doctor: one or more checks failed")

// goos defaults to runtime.GOOS; tests override it so platform-specific
// checks (Xcode Command Line Tools) are deterministic on any host.
var goos = runtime.GOOS

func doctorCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check whether this machine is ready to use envctl",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env := doctor.Env{Run: doctor.ExecRunner, Dir: g.dir, GOOS: goos}
			results, failed := doctor.Report(cmd.Context(), env)
			if g.jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(results); err != nil {
					return err
				}
			} else {
				printDoctorResults(cmd.OutOrStdout(), results)
			}
			if failed {
				return errDoctorFailed
			}
			return nil
		},
	}
}

func printDoctorResults(w io.Writer, results []doctor.Result) {
	for _, r := range results {
		if r.Status == doctor.OK {
			fmt.Fprintf(w, "[%s]      %-16s %s\n", r.Status, r.Name, r.Detail)
			continue
		}
		fmt.Fprintf(w, "[%s] %-16s %s -- fix: %s\n", r.Status, r.Name, r.Detail, r.Fix)
	}
}
