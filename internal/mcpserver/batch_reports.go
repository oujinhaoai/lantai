package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/assettools"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func batchTextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (a *adapter) batchReportTool() {
	mcp.AddTool(a.s, &mcp.Tool{Name: "batch_report_page", Description: "Read bounded metadata rows/files/edits/issues from an exact private batch receipt. Continue with offset and original expected_sha256. Only known import/workset/reference/derivation/comparison/OBJ schemas are readable; source roots, complete notes, transfer grants and credentials are excluded. This is a saved local observation, never current remote authorization or proof; recorded verification flags are explicitly historical.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		File           string `json:"file"`
		Section        string `json:"section,omitempty"`
		Key            string `json:"key,omitempty"`
		Offset         int    `json:"offset,omitempty"`
		Limit          int    `json:"limit,omitempty"`
		ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		fail := func(err error) (*mcp.CallToolResult, any, error) {
			r, e := toolResult(client.Response{}, err)
			return r, nil, e
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if in.Limit == 0 {
			in.Limit = 10
		}
		if in.Section == "" {
			in.Section = "rows"
		}
		if in.Offset < 0 || in.Limit < 1 || in.Limit > 20 || in.Offset > 0 && in.ExpectedSHA256 == "" {
			return readError("invalid receipt page or missing original expected_sha256")
		}
		path, err := a.localPath(in.File, false)
		if err != nil {
			return fail(err)
		}
		raw, err := a.readBatchPrivate(path)
		if err != nil {
			return fail(err)
		}
		if err := canonjson.Validate(raw); err != nil {
			return readError("private receipt must be unambiguous JSON")
		}
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != digest {
			return fail(errcode.New(errcode.PreconditionFailed, "saved receipt changed; replace prior pages"))
		}
		rows, metadata, err := batchReportRows(raw, in.Section, in.Key)
		if err != nil {
			return fail(err)
		}
		if in.Offset > len(rows) {
			return readError("offset exceeds receipt section")
		}
		end := min(in.Offset+in.Limit, len(rows))
		for {
			page := map[string]any{"file": in.File, "section": in.Section, "key": in.Key, "sha256": digest, "offset": in.Offset, "total_items": len(rows), "items": rows[in.Offset:end], "complete": end == len(rows), "historical_local_observation": true, "current_remote_verified": false, "recorded": metadata}
			if end < len(rows) {
				page["next_offset"] = end
			}
			encoded, _ := json.Marshal(page)
			r, e := toolResult(client.Response{Body: encoded}, nil)
			if e != nil || !r.IsError || end <= in.Offset+1 {
				return r, nil, e
			}
			end--
		}
	})
}

func batchReportRows(raw []byte, section, key string) ([]any, map[string]any, error) {
	var header struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, nil, err
	}
	rows := []any{}
	meta := map[string]any{"schema": header.Schema}
	invalid := func() ([]any, map[string]any, error) {
		return nil, nil, errcode.New(errcode.SchemaInvalid, "unknown receipt schema, section or selected row key")
	}
	switch header.Schema {
	case "lantai.version-file-comparison/v1":
		if key != "" {
			return invalid()
		}
		var comparison client.VersionComparison
		if err := json.Unmarshal(raw, &comparison); err != nil {
			return nil, nil, err
		}
		if section == "rows" {
			for _, row := range comparison.Files {
				rows = append(rows, row)
			}
		} else if section == "fields" {
			for _, row := range comparison.Fields {
				before, _ := canonjson.Canonicalize(row.Before)
				after, _ := canonjson.Canonicalize(row.After)
				rows = append(rows, map[string]any{"field": row.Field, "changed": row.Changed, "before_sha256": batchTextHash(string(before)), "after_sha256": batchTextHash(string(after)), "before_bytes": len(row.Before), "after_bytes": len(row.After)})
			}
		} else {
			return invalid()
		}
	case "lantai.obj-structure/v1":
		if key != "" {
			return invalid()
		}
		var object assettools.Wireframe
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, nil, err
		}
		meta["vertices"], meta["edges"], meta["faces"], meta["material_libraries"], meta["material_libraries_truncated"] = len(object.Vertices), len(object.Edges), object.Faces, len(object.MaterialLibraries), object.MaterialLibrariesTruncated
		switch section {
		case "rows":
			rows = append(rows, map[string]any{"vertices": len(object.Vertices), "edges": len(object.Edges), "faces": object.Faces})
		case "vertices":
			for _, point := range object.Vertices {
				rows = append(rows, point)
			}
		case "edges":
			for _, edge := range object.Edges {
				rows = append(rows, edge)
			}
		default:
			return invalid()
		}

	case client.DerivationReceiptSchema:
		if section != "rows" || key != "" {
			return invalid()
		}
		var receipt client.DerivationReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return nil, nil, err
		}
		meta["tool"], meta["action"], meta["purpose"], meta["artifact"], meta["current_remote_verified"] = receipt.Tool, receipt.Action, receipt.Purpose, receipt.Artifact, receipt.CurrentRemoteVerified
		for _, proof := range receipt.Inputs {
			rows = append(rows, proof)
		}

	case client.ImportPlanSchema:
		var plan client.ImportPlan
		if err := json.Unmarshal(raw, &plan); err != nil {
			return nil, nil, err
		}
		meta["plan_digest"], meta["planned"] = plan.Digest, len(plan.Input.Assets)
		if section == "rows" && key == "" {
			for _, a := range plan.Input.Assets {
				rows = append(rows, map[string]any{"key": a.Key, "classification": a.Classification, "project_id": a.Push.ProjectID, "asset_id": a.Push.AssetID, "base_version_id": a.Push.BaseVersionID, "asset_type": a.Push.Content.AssetType, "files": len(a.Push.Content.Files)})
			}
		} else if section == "files" {
			found := false
			for _, a := range plan.Input.Assets {
				if a.Key == key {
					found = true
					for _, f := range a.Push.Content.Files {
						rows = append(rows, f)
					}
					break
				}
			}
			if !found {
				return invalid()
			}
		} else {
			return invalid()
		}
	case client.ImportReportSchema:
		if section != "rows" || key != "" {
			return invalid()
		}
		var report client.ImportReport
		if err := json.Unmarshal(raw, &report); err != nil {
			return nil, nil, err
		}
		meta["plan_digest"], meta["complete"], meta["current_remote_verified"], meta["completion_basis"] = report.PlanDigest, report.Complete, report.CurrentRemoteVerified, report.CompletionBasis
		for _, r := range report.Results {
			rows = append(rows, r)
		}
	case "lantai.asset-import-verify/v1":
		if section != "rows" || key != "" {
			return invalid()
		}
		var report client.ImportVerifyReport
		if err := json.Unmarshal(raw, &report); err != nil {
			return nil, nil, err
		}
		meta["plan_digest"], meta["complete"], meta["current_remote_verified"], meta["completion_basis"] = report.PlanDigest, report.Complete, report.CurrentRemoteVerified, report.CompletionBasis
		for _, r := range report.Results {
			rows = append(rows, r)
		}
	case client.WorksetPlanSchema, client.WorksetReceiptSchema:
		var plan client.WorksetPlan
		if err := json.Unmarshal(raw, &plan); err != nil {
			return nil, nil, err
		}
		meta["input_digest"], meta["state"], meta["files"], meta["bytes"] = plan.InputDigest, plan.State, plan.FileCount, plan.TotalBytes
		if section == "rows" && key == "" {
			for _, r := range plan.Entries {
				rows = append(rows, map[string]any{"key": r.Destination, "asset_id": r.AssetID, "version_id": r.VersionID, "instance_id": r.InstanceID, "destination": r.Destination, "manifest_digest": r.ManifestDigest, "files": len(r.Files)})
			}
		} else if section == "files" {
			found := false
			for _, r := range plan.Entries {
				if r.Destination == key {
					found = true
					for _, f := range r.Files {
						rows = append(rows, f)
					}
					break
				}
			}
			if !found {
				return invalid()
			}
		} else {
			return invalid()
		}
	case client.ReferencePlanSchema:
		var plan client.ReferencePlan
		if err := json.Unmarshal(raw, &plan); err != nil {
			return nil, nil, err
		}
		meta["plan_digest"], meta["complete"], meta["scope"], meta["unresolved"], meta["skipped"] = plan.Digest, plan.Complete, plan.Scope, len(plan.Unresolved), len(plan.Skipped)
		if section == "rows" && key == "" {
			for _, n := range plan.Notes {
				rows = append(rows, map[string]any{"key": n.Path, "path": n.Path, "before_sha256": n.BeforeSHA256, "after_sha256": n.AfterSHA256, "edits": len(n.Edits)})
			}
		} else if section == "edits" {
			found := false
			for _, n := range plan.Notes {
				if n.Path == key {
					found = true
					for _, e := range n.Edits {
						rows = append(rows, map[string]any{"start": e.Start, "end": e.End, "target": e.Target, "fixed_ref": e.FixedRef, "syntax": e.Syntax, "before_sha256": batchTextHash(e.Before), "after_sha256": batchTextHash(e.After), "before_bytes": len(e.Before), "after_bytes": len(e.After)})
					}
					break
				}
			}
			if !found {
				return invalid()
			}
		} else if section == "issues" && key == "" {
			for _, list := range [][]client.ReferenceIssue{plan.Unresolved, plan.Skipped} {
				for _, r := range list {
					rows = append(rows, map[string]any{"note": r.Note, "offset": r.Offset, "reason": r.Reason, "target_sha256": batchTextHash(r.Target)})
				}
			}
		} else {
			return invalid()
		}
	case client.ReferenceReceiptSchema:
		if section != "rows" || key != "" {
			return invalid()
		}
		var report client.ReferenceReceipt
		if err := json.Unmarshal(raw, &report); err != nil {
			return nil, nil, err
		}
		meta["plan_digest"], meta["complete"], meta["scope"], meta["post_cutover_refs"] = report.PlanDigest, report.Complete, report.Scope, len(report.PostCutoverRefs)
		for _, r := range report.Results {
			rows = append(rows, r)
		}
	default:
		return invalid()
	}
	return rows, meta, nil
}
