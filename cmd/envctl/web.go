package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/tui"
	"github.com/sam-bretz/envctl/internal/web"
)

func webCmd(g *globals) *cobra.Command {
	var addr string
	var noOpen bool
	c := &cobra.Command{Use: "web", Short: "Open the workflow dashboard in a browser on this machine", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("--addr: %w", err)
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("--addr must be a loopback address such as 127.0.0.1:4777; the dashboard can approve and publish work")
		}
		dir, err := stateDir(g)
		if err != nil {
			return err
		}
		token, err := web.LoadToken(dir)
		if err != nil {
			return err
		}
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("%w; choose another port with --addr 127.0.0.1:0", err)
		}
		srv := &web.Server{API: client, Root: workflowRoot(cmd.Context(), g.dir), Token: token, Hosts: web.LoopbackHosts(listener.Addr()), Owner: "local"}
		if user := os.Getenv("USER"); user != "" {
			srv.Owner = user
		}
		url := fmt.Sprintf("http://%s/?token=%s", listener.Addr(), token)
		fmt.Fprintf(cmd.OutOrStdout(), "envctl web is serving %s\nOpen: %s\nPress Ctrl+C to stop. Runs keep going without it.\n", srv.Root, url)
		if !noOpen {
			if err := tui.OpenInBrowser(url); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "could not open a browser (%v); open the link above\n", err)
			}
		}
		server := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-cmd.Context().Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = server.Shutdown(ctx)
		}()
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:4777", "loopback address to serve on")
	c.Flags().BoolVar(&noOpen, "no-open", false, "print the link without opening a browser")
	return c
}
