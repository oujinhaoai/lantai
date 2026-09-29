package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
)

func runMCP(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("mcp", flag.ContinueOnError)
	f.SetOutput(stderr)
	server := f.String("server", os.Getenv("LANTAI_SERVER"), "HTTPS REST origin")
	session := f.String("session-file", os.Getenv("LANTAI_SESSION"), "private existing session file")
	workspace := f.String("workspace", "", "existing local directory; enables bounded push/pull")
	registry := f.String("extensions-registry", os.Getenv("LANTAI_EXTENSIONS"), "private local extension registry; projects installed, server-enabled commands (needs -workspace)")
	allowHTTP := f.Bool("allow-http", false, "allow loopback development HTTP")
	if e := f.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 {
		return 2
	}
	c, e := client.New(client.Config{BaseURL: *server, AllowHTTP: *allowHTTP})
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 2
	}
	defer c.Close()
	token := os.Getenv("LANTAI_SESSION_TOKEN")
	if token == "" && *session != "" {
		var saved struct {
			Schema string `json:"schema"`
			Origin string `json:"origin"`
			Token  string `json:"token"`
		}
		if e = client.LoadState(*session, &saved); e != nil {
			fmt.Fprintln(stderr, "mcp: session file unavailable")
			return 3
		}
		if saved.Schema != "lantai.client-session/v1" || saved.Origin != c.Origin() {
			fmt.Fprintln(stderr, "mcp: session origin mismatch")
			return 2
		}
		token = saved.Token
	}
	if token == "" {
		fmt.Fprintln(stderr, "mcp: existing session required")
		return 5
	}
	if e = c.SetSessionToken(token); e != nil {
		return 2
	}
	if *registry != "" {
		if abs, err := filepath.Abs(*registry); err == nil {
			*registry = abs
		}
	}
	s, e := mcpserver.New(ctx, mcpserver.Config{Client: c, Workspace: *workspace, ExtensionsRegistry: *registry})
	if e != nil {
		fmt.Fprintln(stderr, "mcp: cannot initialize REST adapter:", e)
		return 3
	}
	if e = s.Run(ctx, &mcp.StdioTransport{MaxLineLength: mcpserver.MaxRequestBytes}); e != nil && !errors.Is(e, context.Canceled) {
		fmt.Fprintln(stderr, "mcp: transport stopped")
		return 3
	}
	return 0
}
