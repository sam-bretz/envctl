package main

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sam-bretz/envctl/internal/mcpserver"
	"github.com/spf13/cobra"
)

func mcpCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "mcp", Short: "Connect local agent clients to workflow commands"}
	options := mcpserver.Options{Version: version}
	serve := &cobra.Command{Use: "serve", Short: "Serve MCP over stdin/stdout using the persistent coordinator", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		return mcpserver.New(client, options).Run(cmd.Context(), &mcp.StdioTransport{})
	}}
	serve.Flags().StringVar(&options.Run, "run", "", "restrict reads and mutations to one run; disable creation")
	serve.Flags().BoolVar(&options.ReadOnly, "read-only", false, "expose only workflow inspection tools")
	c.AddCommand(serve)
	return c
}
