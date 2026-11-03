package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

type BatchArgs struct {
	InputFile           string `json:"input_file"`
	Directory           string `json:"directory"`
	OutputFile          string `json:"output_file"`
	StateDirectory      string `json:"state_directory,omitempty"`
	Destination         string `json:"destination,omitempty"`
	ReportFile          string `json:"report_file,omitempty"`
	Purpose             string `json:"purpose,omitempty"`
	PostCutoverRefsFile string `json:"post_cutover_refs_file,omitempty"`
}

func (a *adapter) localBatchDirectory(relative string) (string, error) {
	if path, err := a.localDirectory(relative); err == nil {
		return path, nil
	}
	path, err := a.localPath(relative, true)
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", errcode.New(errcode.PathConflict, "batch directory must be an explicit workspace directory")
	}
	return path, nil
}

func (a *adapter) batchTools(has func(string) bool) {
	a.batchReportTool()
	for _, tool := range []struct {
		name, cap, description string
		plan                   bool
	}{
		{"import_plan", "uploads", "Hash an explicit private JSON import selection inside directory, without remote writes. Saves an immutable original plan outside the source and returns only bounded counts/hash; inspect it before import_apply.", true},
		{"import_apply", "uploads", "Apply only the original reviewed import plan with its original private state_directory. Saves every partial historical receipt; unknown and description_pending never cause replacement keys. Input/source changes fail closed. Output is a bounded receipt; verify actual remote bytes separately.", false},
		{"import_verify", "exact_read", "Recheck the original plan, report and state against current remote permissions/purpose and actual downloaded SHA/size in an independent destination. Explicit purpose required. Historical receipts alone cannot set current_remote_verified.", false},
		{"workset_plan", "exact_read", "Plan exact workset references, destinations, entrypoints and declared dependencies without transfer grants or file writes. Save an immutable plan outside the workset; no native app is launched.", true},
		{"workset_pull", "exact_read", "Pull a fixed workset input into directory using current purpose/authority and exact byte verification. Existing different files are never overwritten; original inputs resume. Saves a private complete/partial receipt outside the workset.", false},
		{"references_plan", "exact_read", "Plan only explicitly selected note links with exact fixed mappings; preserve original text, diff, skipped and unresolved entries in an immutable private plan. No source edits or complete note text are returned in context.", true},
		{"references_copy", "exact_read", "Create independent note copies from the original exact reference plan. Never edit source notes or infer mappings. Preserve existing copy state and original plan on partial/unknown results.", false},
		{"references_rollback", "exact_read", "Create independent original-note copies from the exact original plan and managed copy directory, retaining explicitly selected post_cutover_refs_file. Never replace the original notes, modified copies or a prior rollback.", false},
	} {
		if !has(tool.cap) {
			continue
		}
		mcp.AddTool(a.s, &mcp.Tool{Name: tool.name, Description: tool.description}, func(ctx context.Context, _ *mcp.CallToolRequest, in BatchArgs) (*mcp.CallToolResult, any, error) {
			return a.executeBatch(ctx, tool.name, tool.plan, in)
		})
	}
}

func (a *adapter) executeBatch(ctx context.Context, action string, immutable bool, in BatchArgs) (*mcp.CallToolResult, any, error) {
	fail := func(err error) (*mcp.CallToolResult, any, error) {
		r, e := toolResult(client.Response{}, err)
		return r, nil, e
	}
	if err := a.checkBatchWorkspace(); err != nil {
		return fail(err)
	}
	input, err := a.localPath(in.InputFile, false)
	if err != nil {
		return fail(err)
	}
	var directory string
	if action == "workset_plan" || action == "workset_pull" {
		directory, err = a.localBatchDirectory(in.Directory)
	} else {
		directory, err = a.localDirectory(in.Directory)
	}
	if err != nil {
		return fail(err)
	}
	outputPath, err := a.localPath(in.OutputFile, true)
	if err != nil {
		return fail(err)
	}
	protected := []string{directory, input}
	var state, destination, priorReport, cutover string
	if action == "import_apply" || action == "import_verify" {
		state, err = a.localBatchDirectory(in.StateDirectory)
		if err != nil {
			return fail(err)
		}
		protected = append(protected, state)
	}
	if action == "import_verify" || action == "references_copy" || action == "references_rollback" {
		destination, err = a.localBatchDirectory(in.Destination)
		if err != nil {
			return fail(err)
		}
		protected = append(protected, destination)
	}
	if action == "import_verify" {
		priorReport, err = a.localPath(in.ReportFile, false)
		if err != nil {
			return fail(err)
		}
		protected = append(protected, priorReport)
	}
	if action == "references_rollback" {
		cutover, err = a.localPath(in.PostCutoverRefsFile, false)
		if err != nil {
			return fail(err)
		}
		protected = append(protected, cutover)
	}
	// A rollback's original source is in its frozen plan, not directory.
	var reference client.ReferencePlan
	if action == "references_copy" || action == "references_rollback" {
		if err := a.loadBatchPrivate(input, &reference); err != nil {
			return fail(err)
		}
		original, err := a.workspaceStoredDirectory(reference.SourceRoot)
		if err != nil {
			return fail(err)
		}
		protected = append(protected, original)
	}
	output, err := client.OpenBatchPrivateOutput(outputPath, protected...)
	if err != nil {
		return fail(err)
	}
	defer output.Close()
	if err := a.checkBatchWorkspace(); err != nil {
		return fail(err)
	}
	var value any
	switch action {
	case "import_plan":
		var selection client.ImportInput
		if err = a.loadBatchPrivate(input, &selection); err == nil {
			value, err = client.PrepareImport(ctx, selection, directory)
		}
	case "import_apply", "import_verify":
		var plan client.ImportPlan
		if err = a.loadBatchPrivate(input, &plan); err != nil {
			return fail(err)
		}
		if _, err = a.workspaceStoredDirectory(plan.SourceRoot); err != nil {
			return fail(err)
		}
		if action == "import_apply" {
			value, err = a.c.ApplyImport(ctx, plan, directory, state)
		} else {
			var report client.ImportReport
			if err = a.loadBatchPrivate(priorReport, &report); err != nil {
				return fail(err)
			}
			value, err = a.c.VerifyImport(ctx, plan, report, directory, state, destination, in.Purpose)
		}
	case "workset_plan", "workset_pull":
		var selection client.WorksetInput
		if err = a.loadBatchPrivate(input, &selection); err != nil {
			return fail(err)
		}
		if action == "workset_plan" {
			value, err = a.c.PlanWorkset(ctx, selection, directory)
		} else {
			if in.Purpose == "" {
				return fail(errcode.New(errcode.SchemaInvalid, "explicit workset purpose required"))
			}
			value, err = a.c.PullWorkset(ctx, selection, directory, in.Purpose)
		}
	case "references_plan":
		var selection client.ReferenceInput
		if err = a.loadBatchPrivate(input, &selection); err != nil {
			return fail(err)
		}
		var plan client.ReferencePlan
		plan, err = client.PrepareReferencePlan(ctx, selection, directory)
		value = plan
		if err == nil && !plan.Complete {
			err = errcode.New(errcode.PreconditionFailed, "selected mappings unresolved; inspect the saved original plan")
		}
	case "references_copy":
		value, err = client.CopyReferenceNotes(ctx, reference, directory, destination)
	case "references_rollback":
		var refs []string
		if err = a.loadBatchPrivate(cutover, &refs); err != nil {
			return fail(err)
		}
		value, err = client.RollbackReferenceNotes(ctx, reference, directory, destination, refs)
	default:
		return fail(errcode.New(errcode.UnsupportedCapability, "unsupported fixed batch action"))
	}
	if value == nil {
		return fail(err)
	}
	summary := batchSummary(value)
	if schema, _ := summary["schema"].(string); schema == "" {
		if err == nil {
			err = errcode.New(errcode.Internal, "batch owner omitted its result schema")
		}
		return fail(err)
	}
	raw, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return fail(marshalErr)
	}
	raw = append(raw, '\n')
	var writeErr error
	if err := a.checkBatchWorkspace(); err != nil {
		return fail(err)
	}
	if immutable {
		writeErr = output.WriteOriginal(raw)
	} else {
		writeErr = output.Write(raw)
	}
	if writeErr != nil {
		return fail(writeErr)
	}
	sum := sha256.Sum256(raw)
	summary["receipt_file"], summary["receipt_sha256"], summary["receipt_bytes"] = in.OutputFile, hex.EncodeToString(sum[:]), len(raw)
	if err != nil {
		r, _ := toolResult(client.Response{}, err)
		var problem map[string]any
		encoded, _ := json.Marshal(r.StructuredContent)
		_ = json.Unmarshal(encoded, &problem)
		problem["receipt"] = summary
		encoded, _ = json.Marshal(problem)
		r, e := toolResult(client.Response{Body: encoded}, nil)
		r.IsError = true
		return r, nil, e
	}
	encoded, _ := json.Marshal(summary)
	r, e := toolResult(client.Response{Body: encoded}, nil)
	return r, nil, e
}

func (a *adapter) checkBatchWorkspace() error {
	current, err := os.Lstat(a.workspace)
	if err != nil || !current.IsDir() || a.workspaceIdentity == nil || !os.SameFile(a.workspaceIdentity, current) {
		return errcode.New(errcode.PathConflict, "configured workspace replaced; preserve original state")
	}
	return nil
}

func (a *adapter) workspaceStoredDirectory(absolute string) (string, error) {
	if absolute == "" || !filepath.IsAbs(absolute) {
		return "", errcode.New(errcode.SchemaInvalid, "original plan root must be an exact workspace directory")
	}
	rel, err := filepath.Rel(a.workspace, absolute)
	if err != nil {
		return "", err
	}
	return a.localDirectory(rel)
}

func (a *adapter) loadBatchPrivate(path string, value any) error {
	raw, err := a.readBatchPrivate(path)
	if err != nil {
		return err
	}
	if err := canonjson.Validate(raw); err != nil {
		return errcode.New(errcode.SchemaInvalid, "batch input must be unambiguous JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func (a *adapter) readBatchPrivate(path string) ([]byte, error) {
	relative, err := filepath.Rel(a.workspace, path)
	if err != nil {
		return nil, err
	}
	if _, err := a.localPath(relative, false); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(a.workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	current, err := root.Stat(".")
	if err != nil || a.workspaceIdentity == nil || !os.SameFile(a.workspaceIdentity, current) {
		return nil, errcode.New(errcode.PathConflict, "configured workspace replaced; preserve original state")
	}
	raw, err := client.ReadPrivateFromRoot(root, relative)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func batchSummary(value any) map[string]any {
	out := map[string]any{"saved": true}
	switch v := value.(type) {
	case client.ImportPlan:
		out["schema"], out["plan_digest"], out["planned"], out["selected_files"], out["selected_bytes"], out["uncovered"] = v.Schema, v.Digest, len(v.Input.Assets), v.SelectedFiles, v.SelectedBytes, len(v.Uncovered)
	case client.ImportReport:
		counts := map[string]int{}
		for _, r := range v.Results {
			counts[r.Status]++
		}
		out["schema"], out["plan_digest"], out["complete"], out["completion_basis"], out["current_remote_verified"], out["counts"] = v.Schema, v.PlanDigest, v.Complete, v.CompletionBasis, v.CurrentRemoteVerified, counts
	case client.ImportVerifyReport:
		counts := map[string]int{}
		for _, r := range v.Results {
			counts[r.Status]++
		}
		out["schema"], out["plan_digest"], out["complete"], out["completion_basis"], out["current_remote_verified"], out["counts"] = v.Schema, v.PlanDigest, v.Complete, v.CompletionBasis, v.CurrentRemoteVerified, counts
	case client.WorksetPlan:
		out["schema"], out["input_digest"], out["state"], out["files"], out["bytes"] = v.Schema, v.InputDigest, v.State, v.FileCount, v.TotalBytes
	case client.WorksetReceipt:
		return batchSummary(client.WorksetPlan(v))
	case client.ReferencePlan:
		out["schema"], out["plan_digest"], out["complete"], out["notes"], out["unresolved"], out["skipped"], out["scope"] = v.Schema, v.Digest, v.Complete, len(v.Notes), len(v.Unresolved), len(v.Skipped), v.Scope
	case client.ReferenceReceipt:
		counts := map[string]int{}
		for _, r := range v.Results {
			counts[r.State]++
		}
		out["schema"], out["plan_digest"], out["complete"], out["scope"], out["counts"], out["post_cutover_refs"] = v.Schema, v.PlanDigest, v.Complete, v.Scope, counts, len(v.PostCutoverRefs)
	}
	return out
}
