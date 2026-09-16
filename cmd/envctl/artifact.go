package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/tui"
)

func runArtifactCmd(g *globals) *cobra.Command {
	var output string
	var open bool
	c := &cobra.Command{Use: "artifact <digest>", Short: "Export or open a checksum-verified checkpoint artifact", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if output == "" && !open {
			return errors.New("either --output or --open is required")
		}
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		raw, err := client.Artifact(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if output != "" {
			filename, err := filepath.Abs(output)
			if err != nil {
				return err
			}
			if err = exportArtifact(filename, raw); err != nil {
				return err
			}
			if !open {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"path": filename, "digest": args[0], "bytes": len(raw)})
			}
		}
		session, _, err := startWebSession(cmd.Context(), g, client, "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("could not start the artifact browser: %w", err)
		}
		url := session.ArtifactURL(args[0])
		fmt.Fprintf(cmd.OutOrStdout(), "Open: %s\nPress Ctrl+C to stop.\n", url)
		if err := tui.OpenInBrowser(url); err != nil {
			return fmt.Errorf("could not open a browser (%v); open the link above", err)
		}
		return session.Wait()
	}}
	c.Flags().StringVarP(&output, "output", "o", "", "new destination file (existing paths are never overwritten)")
	c.Flags().BoolVar(&open, "open", false, "open the verified artifact in the local browser")
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
