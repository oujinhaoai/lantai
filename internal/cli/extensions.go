package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/extensions/local"
)

// pluginCommand is the governance face (T09): static import, diagnostics,
// probe rerun and the caller's enabled command list. Enable and disable are
// HumanGrant-bound and go through `human prepare` with the extension kinds.
func pluginCommand(ctx context.Context, c *client.Client, action string, o options) (client.Response, error) {
	opts := client.Options{IdempotencyKey: o.key}
	switch action {
	case "import":
		if err := needed(o.key, "--idempotency-key is required; preserve it for retries"); err != nil {
			return client.Response{}, err
		}
		body, err := input(o.input)
		if err != nil {
			return client.Response{}, err
		}
		return c.Do(ctx, http.MethodPost, "/api/v1/extensions/packages", body, opts)
	case "", "list":
		return c.Do(ctx, http.MethodGet, "/api/v1/extensions/packages", nil, opts)
	case "enablements":
		return c.Do(ctx, http.MethodGet, "/api/v1/extensions/enablements", nil, opts)
	case "probe":
		if err := needed(o.id, "--id enablement ID is required"); err != nil {
			return client.Response{}, err
		}
		return c.Do(ctx, http.MethodPost, "/api/v1/extensions/enablements/"+segment(o.id)+"/probe", nil, opts)
	case "commands":
		return c.Do(ctx, http.MethodGet, "/api/v1/extensions/commands", nil, opts)
	}
	return client.Response{}, usage("plugin requires import, list, enablements, probe or commands")
}

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// extCommand is the explicit local execution face: install records trust in an
// exact package directory, run executes one installed command. Only fully
// qualified `ext run <plugin-id> <command>` exists, so a plugin cannot shadow
// a core command, and nothing is looked up in PATH or the working directory.
func extCommand(ctx context.Context, args []string) (client.Response, error) {
	if len(args) == 0 {
		return client.Response{}, usage("ext requires install, list, remove or run")
	}
	action, rest := args[0], args[1:]
	positional := []string{}
	if action == "run" {
		for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && len(positional) < 2 {
			positional, rest = append(positional, rest[0]), rest[1:]
		}
	}
	var o options
	var registry, pkg, output, plugin string
	var inputs, argv repeated
	f := flag.NewFlagSet("ext", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.server, "server", os.Getenv("LANTAI_SERVER"), "HTTPS gateway origin")
	f.StringVar(&o.session, "session-file", os.Getenv("LANTAI_SESSION"), "private session file")
	f.StringVar(&o.caFile, "ca-file", os.Getenv("LANTAI_CA_FILE"), "PEM CA bundle for this client only")
	f.BoolVar(&o.allowHTTP, "allow-http", false, "allow loopback HTTP for development")
	f.BoolVar(&o.json, "json", false, "stable JSON output (default)")
	f.StringVar(&registry, "registry", os.Getenv("LANTAI_EXTENSIONS"), "private local extension registry file")
	f.StringVar(&pkg, "package", "", "package directory to trust (install)")
	f.StringVar(&plugin, "plugin", "", "extension ID (remove)")
	f.StringVar(&output, "output", "", "existing empty output directory (run)")
	f.Var(&inputs, "input-file", "explicitly selected input file (run, repeatable)")
	f.Var(&argv, "arg", "one verbatim argument (run, repeatable; never a shell string)")
	if err := f.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return client.Response{}, err
		}
		return client.Response{}, usage("invalid ext arguments; use --help")
	}
	if f.NArg() != 0 {
		return client.Response{}, usage("unexpected positional arguments")
	}
	abs := func(p string) (string, error) {
		if p == "" {
			return "", nil
		}
		return filepath.Abs(p)
	}
	var err error
	if registry, err = abs(registry); err != nil || registry == "" {
		return client.Response{}, usage("--registry FILE (or LANTAI_EXTENSIONS) is required")
	}
	c, err := connect(o)
	if err != nil {
		return client.Response{}, err
	}
	defer c.Close()
	var out any
	switch action {
	case "install":
		if pkg == "" {
			return client.Response{}, usage("--package DIR is required")
		}
		out, err = local.Install(ctx, c, registry, pkg)
	case "list":
		var reg local.Registry
		reg, err = local.Load(registry, c.Origin())
		out = map[string]any{"items": reg.Entries}
	case "remove":
		if plugin == "" {
			return client.Response{}, usage("--plugin ID is required")
		}
		err = local.Remove(registry, c.Origin(), plugin)
		out = map[string]any{"removed": plugin}
	case "run":
		if len(positional) != 2 {
			return client.Response{}, usage("ext run <plugin-id> <command> is required")
		}
		if output, err = abs(output); err != nil || output == "" {
			return client.Response{}, usage("--output DIR is required")
		}
		files := []string{}
		for _, p := range inputs {
			a, err := abs(p)
			if err != nil {
				return client.Response{}, err
			}
			files = append(files, a)
		}
		var res local.RunResult
		res, err = local.Run(ctx, c, registry, local.RunRequest{ExtensionID: positional[0], Command: positional[1], Args: argv, Inputs: files, Output: output})
		out = res
		if err == nil && res.Failure != "" {
			err = errcode.New(errcode.OperationNeedsReconciliation, "extension command failed: "+res.Failure)
		}
	default:
		return client.Response{}, usage("ext requires install, list, remove or run")
	}
	if err != nil {
		return client.Response{}, err
	}
	b, err := json.Marshal(out)
	return client.Response{Status: 200, Body: b}, err
}

// connect opens an authenticated REST client from the shared options.
func connect(o options) (*client.Client, error) {
	if o.server == "" {
		return nil, usage("--server or LANTAI_SERVER is required")
	}
	c, err := client.New(client.Config{BaseURL: o.server, AllowHTTP: o.allowHTTP, CAFile: o.caFile})
	if err != nil {
		return nil, usage(err.Error())
	}
	token := os.Getenv("LANTAI_SESSION_TOKEN")
	if token == "" && o.session != "" {
		var saved sessionFile
		if err = client.LoadState(o.session, &saved); err != nil {
			c.Close()
			return nil, err
		}
		if saved.Schema != "lantai.client-session/v1" || saved.Origin != c.Origin() {
			c.Close()
			return nil, usage("session file belongs to a different server")
		}
		token = saved.Token
	}
	if token == "" {
		c.Close()
		return nil, errcode.New(errcode.AuthRequired, "session credential is required")
	}
	if err = c.SetSessionToken(token); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
