package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

type PushArgs struct {
	Directory string           `json:"directory"`
	State     string           `json:"state"`
	Input     client.PushInput `json:"input"`
}
type PullArgs struct {
	Directory string `json:"directory"`
	AssetID   string `json:"asset_id"`
	VersionID string `json:"version_id"`
	Purpose   string `json:"purpose"`
}

func (a *adapter) localPath(relative string, createFile bool) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return "", errcode.New(errcode.SchemaInvalid, "workspace-relative path required")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errcode.New(errcode.SchemaInvalid, "path escapes workspace")
	}
	path := filepath.Join(a.workspace, clean)
	check := path
	pathError := "local path must exist and be accessible"
	if createFile {
		check = filepath.Dir(path)
		pathError = "local state parent must be an existing accessible directory; create it inside the workspace or choose an existing parent"
	}
	actual, e := filepath.EvalSymlinks(check)
	if e != nil {
		return "", errcode.New(errcode.SchemaInvalid, pathError)
	}
	rel, e := filepath.Rel(a.workspace, actual)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errcode.New(errcode.SchemaInvalid, "path escapes workspace")
	}
	if createFile {
		if info, e := os.Stat(actual); e != nil || !info.IsDir() {
			return "", errcode.New(errcode.SchemaInvalid, pathError)
		}
		if stat, e := os.Lstat(path); e == nil && stat.Mode()&os.ModeSymlink != 0 {
			return "", errcode.New(errcode.SchemaInvalid, "state symlink forbidden")
		}
	}
	return path, nil
}

// Extension inputs also use localPath and may be files. Only the push/pull
// workspace roots and state parents require a directory.
func (a *adapter) localDirectory(relative string) (string, error) {
	path, err := a.localPath(relative, false)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", errcode.New(errcode.SchemaInvalid, "local directory must be an existing accessible directory")
	}
	return path, nil
}

func (a *adapter) localTools() error {
	root, e := filepath.Abs(a.workspace)
	if e != nil {
		return e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	info, e := os.Stat(root)
	if e != nil || !info.IsDir() {
		return errors.New("mcp: existing workspace directory required")
	}
	a.workspace = root
	inputSchema, e := a.pushInputSchema()
	if e != nil {
		return e
	}
	pullSchema, e := a.pullInputSchema()
	if e != nil {
		return e
	}
	mcp.AddTool(a.s, &mcp.Tool{Name: "resource_push", InputSchema: inputSchema, Description: "Hash and stream files inside the configured workspace, using a persistent relative state file for resumable upload and idempotent commit. A receipt with description_pending=true has already committed the files: read/pull its exact asset_id/version_id and do not start another upload. Initial metadata also requires organize scope. Does not approve or publish."}, func(ctx context.Context, _ *mcp.CallToolRequest, in PushArgs) (*mcp.CallToolResult, any, error) {
		directory, e := a.localDirectory(in.Directory)
		if e != nil {
			r, e := toolResult(client.Response{}, e)
			return r, nil, e
		}
		state, e := a.localPath(in.State, true)
		if e != nil {
			r, e := toolResult(client.Response{}, e)
			return r, nil, e
		}
		out, e := a.c.Push(ctx, in.Input, directory, state, false)
		r, e := toolResult(out, e)
		return r, nil, e
	})
	mcp.AddTool(a.s, &mcp.Tool{Name: "resource_pull", InputSchema: pullSchema, Description: "Stream an exact version into an existing relative workspace directory. Select purpose from the advertised enum for the intended use. Grants, paths, size and hashes are verified; signed transfer URLs and file bytes stay out of tool output."}, func(ctx context.Context, _ *mcp.CallToolRequest, in PullArgs) (*mcp.CallToolResult, any, error) {
		directory, e := a.localDirectory(in.Directory)
		if e != nil {
			r, e := toolResult(client.Response{}, e)
			return r, nil, e
		}
		_, e = a.c.Pull(ctx, in.AssetID, in.VersionID, directory, in.Purpose)
		b, _ := json.Marshal(map[string]string{"asset_id": in.AssetID, "version_id": in.VersionID, "directory": in.Directory})
		r, e := toolResult(client.Response{Body: b}, e)
		return r, nil, e
	})
	return nil
}
