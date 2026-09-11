package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func runArtifactCmd(g *globals) *cobra.Command {
	var output string
	c := &cobra.Command{Use: "artifact <digest>", Short: "Export a checksum-verified checkpoint artifact to a new file", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		raw, err := client.Artifact(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		filename, err := filepath.Abs(output)
		if err != nil {
			return err
		}
		if err = exportArtifact(filename, raw); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"path": filename, "digest": args[0], "bytes": len(raw)})
	}}
	c.Flags().StringVarP(&output, "output", "o", "", "new destination file (existing paths are never overwritten)")
	_ = c.MarkFlagRequired("output")
	return c
}

func exportArtifact(filename string, raw []byte) error {
	parent := filepath.Dir(filename)
	f, err := os.CreateTemp(parent, ".envctl-artifact-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	if err = errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	// Linking an already synced file publishes it atomically and refuses both
	// existing files and symlinks. Temp and destination have the same parent.
	if err = os.Link(f.Name(), filename); err != nil {
		return err
	}
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
