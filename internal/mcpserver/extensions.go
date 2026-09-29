package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/extensions/local"
)

// ExtArgs are workspace-relative files, verbatim arguments and an existing
// empty output directory. No shell string, host path or credential is accepted.
type ExtArgs struct {
	Inputs []string `json:"inputs"`
	Args   []string `json:"args"`
	Output string   `json:"output"`
}

var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// extensionTools projects locally installed commands that the server still
// enables for this session. Each call goes through local.Run, which re-verifies
// package/entry digests and current enablement, exactly like `lantai ext run`.
// Annotations derive from the verified contract: outputs are additive files in
// an empty directory, the world is closed, and nothing is idempotent.
func (a *adapter) extensionTools(ctx context.Context, registry string) error {
	if a.workspace == "" {
		return errors.New("mcp: extension commands require a workspace")
	}
	reg, err := local.Load(registry, a.c.Origin())
	if err != nil {
		return err
	}
	enabled, err := local.Enabled(ctx, a.c)
	if err != nil {
		return err
	}
	no := false
	for _, entry := range reg.Entries {
		for _, contribution := range entry.Commands {
			if !slices.ContainsFunc(enabled, func(c local.Command) bool {
				return c.ContributionID == contribution && c.PackageDigest == entry.PackageDigest
			}) {
				continue
			}
			name := "ext_" + strings.ReplaceAll(contribution, ".", "_")
			if !toolNameRE.MatchString(name) {
				return errors.New("mcp: extension command name exceeds the MCP tool name limit")
			}
			entry, command := entry, strings.TrimPrefix(contribution, entry.ExtensionID+".")
			mcp.AddTool(a.s, &mcp.Tool{Name: name, Description: "Run the locally installed, server-enabled extension command " + contribution + " once on workspace files. Writes only verified outputs into an existing empty workspace directory; it cannot commit, approve, publish or read credentials.", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &no}}, func(ctx context.Context, _ *mcp.CallToolRequest, in ExtArgs) (*mcp.CallToolResult, any, error) {
				req := local.RunRequest{ExtensionID: entry.ExtensionID, Command: command, Args: in.Args}
				for _, rel := range in.Inputs {
					p, e := a.localPath(rel, false)
					if e != nil {
						r, e := toolResult(client.Response{}, e)
						return r, nil, e
					}
					req.Inputs = append(req.Inputs, p)
				}
				out, e := a.localPath(in.Output, false)
				if e != nil {
					r, e := toolResult(client.Response{}, e)
					return r, nil, e
				}
				req.Output = out
				res, e := local.Run(ctx, a.c, registry, req)
				if e == nil && res.Failure != "" {
					e = errcode.New(errcode.OperationNeedsReconciliation, "extension command failed: "+res.Failure)
				}
				b, _ := json.Marshal(res)
				r, e := toolResult(client.Response{Body: b}, e)
				return r, nil, e
			})
		}
	}
	return nil
}
