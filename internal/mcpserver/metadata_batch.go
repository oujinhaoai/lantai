package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func (a *adapter) metadataBatchTools() {
	check := a.checkBatchWorkspace
	load := func(relative string, value any) error {
		if err := check(); err != nil {
			return err
		}
		path, err := a.localPath(relative, false)
		if err != nil {
			return err
		}
		return a.loadBatchPrivate(path, value)
	}
	for _, name := range []string{"metadata_batch_plan", "metadata_batch_apply"} {
		mcp.AddTool(a.s, &mcp.Tool{Name: name, Description: "Explicit ordered metadata batch using private input_file/output_file and original state_file for apply. Plan only reads current revisions and freezes exact AssetPatch values; apply uses per-asset original conditional PATCH/key, reconciles original receipts, then freshly verifies current full metadata. Not a batch transaction. Preserve every partial/conflict/unknown row and original keys; no automatic revision refresh, rights changes or overwrite of other writers. Large plans/results stay private, bounded context returns counts and original digest.", Annotations: &mcp.ToolAnnotations{IdempotentHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			InputFile  string `json:"input_file"`
			OutputFile string `json:"output_file"`
			StateFile  string `json:"state_file,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			fail := func(err error) (*mcp.CallToolResult, any, error) {
				r, e := toolResult(client.Response{}, err)
				return r, nil, e
			}
			if err := check(); err != nil {
				return fail(err)
			}
			input, err := a.localPath(in.InputFile, false)
			if err != nil {
				return fail(err)
			}
			outputPath, err := a.localPath(in.OutputFile, true)
			if err != nil {
				return fail(err)
			}
			protected := []string{input}
			state := ""
			if name == "metadata_batch_apply" {
				state, err = a.localPath(in.StateFile, true)
				if err != nil {
					return fail(err)
				}
				protected = append(protected, state)
			}
			output, err := client.OpenBatchPrivateOutput(outputPath, protected...)
			if err != nil {
				return fail(err)
			}
			defer output.Close()
			var value any
			summary := map[string]any{}
			if name == "metadata_batch_plan" {
				var selection client.MetadataBatchInput
				if err := load(in.InputFile, &selection); err != nil {
					return fail(err)
				}
				var plan client.MetadataBatchPlan
				plan, err = a.c.PlanMetadataBatch(ctx, selection)
				value = plan
				if err != nil {
					return fail(err)
				}
				summary["schema"], summary["plan_digest"], summary["planned"] = plan.Schema, plan.Digest, len(plan.Items)
			} else {
				var plan client.MetadataBatchPlan
				if err := load(in.InputFile, &plan); err != nil {
					return fail(err)
				}
				var report client.MetadataBatchReport
				report, err = a.c.ApplyMetadataBatch(ctx, plan, state)
				value = report
				counts := map[string]int{}
				for _, row := range report.Results {
					counts[row.Status]++
				}
				summary["schema"], summary["plan_digest"], summary["complete"], summary["current_remote_verified"], summary["counts"] = report.Schema, report.PlanDigest, report.Complete, report.CurrentRemoteVerified, counts
			}
			raw, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				return fail(marshalErr)
			}
			raw = append(raw, '\n')
			if err := check(); err != nil {
				return fail(err)
			}
			var saveErr error
			if name == "metadata_batch_plan" {
				saveErr = output.WriteOriginal(raw)
			} else {
				saveErr = output.Write(raw)
			}
			if saveErr != nil {
				return fail(saveErr)
			}
			sum := sha256.Sum256(raw)
			summary["receipt_file"], summary["receipt_sha256"], summary["receipt_bytes"] = in.OutputFile, hex.EncodeToString(sum[:]), len(raw)
			if err != nil {
				failed, _ := toolResult(client.Response{}, err)
				raw, _ = json.Marshal(failed.StructuredContent)
				var problem map[string]any
				_ = json.Unmarshal(raw, &problem)
				problem["receipt"] = summary
				raw, _ = json.Marshal(problem)
				r, e := toolResult(client.Response{Body: raw}, nil)
				r.IsError = true
				return r, nil, e
			}
			raw, _ = json.Marshal(summary)
			r, e := toolResult(client.Response{Body: raw}, nil)
			return r, nil, e
		})
	}
	mcp.AddTool(a.s, &mcp.Tool{Name: "metadata_batch_report_page", Description: "Read bounded metadata rows from a private frozen metadata plan/report. No patch values, unknown extra or credentials are returned. Nonzero offset requires original expected_sha256; saved verification flags are historical only. This never reauthorizes or verifies current remote state.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		File           string `json:"file"`
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
		if err := check(); err != nil {
			return fail(err)
		}
		if in.Limit == 0 {
			in.Limit = 10
		}
		if in.Offset < 0 || in.Limit < 1 || in.Limit > 20 || in.Offset > 0 && in.ExpectedSHA256 == "" {
			return readError("invalid metadata page or missing original expected_sha256")
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
			return fail(err)
		}
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != hash {
			return fail(errcode.New(errcode.PreconditionFailed, "saved metadata receipt changed; replace prior pages"))
		}
		var header struct {
			Schema string `json:"schema"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return fail(err)
		}
		rows := []any{}
		decode := func(value any) error {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			return decoder.Decode(value)
		}
		recorded := map[string]any{"schema": header.Schema}
		switch header.Schema {
		case client.MetadataBatchPlanSchema:
			var plan client.MetadataBatchPlan
			if err := decode(&plan); err != nil {
				return fail(err)
			}
			recorded["plan_digest"] = plan.Digest
			for _, item := range plan.Items {
				var patch map[string]json.RawMessage
				if err := json.Unmarshal(item.Patch, &patch); err != nil {
					return fail(err)
				}
				fields := []string{}
				for key := range patch {
					fields = append(fields, key)
				}
				slices.Sort(fields)
				canonical, err := canonjson.Canonicalize(item.Patch)
				if err != nil {
					return fail(err)
				}
				sum := sha256.Sum256(canonical)
				rows = append(rows, map[string]any{"asset_id": item.AssetID, "project_id": item.ProjectID, "revision": item.Revision, "if_match": item.IfMatch, "patch_fields": fields, "patch_sha256": hex.EncodeToString(sum[:])})
			}
		case client.MetadataBatchReportSchema:
			var report client.MetadataBatchReport
			if err := decode(&report); err != nil {
				return fail(err)
			}
			recorded["plan_digest"], recorded["complete"], recorded["current_remote_verified"] = report.PlanDigest, report.Complete, report.CurrentRemoteVerified
			for _, row := range report.Results {
				rows = append(rows, row)
			}
		default:
			return readError("only known metadata plan/report schemas can be read")
		}
		if in.Offset > len(rows) {
			return readError("offset exceeds original metadata rows")
		}
		end := min(in.Offset+in.Limit, len(rows))
		for {
			page := map[string]any{"items": rows[in.Offset:end], "total_items": len(rows), "offset": in.Offset, "sha256": hash, "complete": end == len(rows), "historical_local_observation": true, "current_remote_verified": false, "recorded": recorded}
			if end < len(rows) {
				page["next_offset"] = end
			}
			raw, _ = json.Marshal(page)
			r, e := toolResult(client.Response{Body: raw}, nil)
			if e != nil || !r.IsError || end <= in.Offset+1 {
				return r, nil, e
			}
			end--
		}
	})
}
