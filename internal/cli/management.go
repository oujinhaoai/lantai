package cli

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/oujinhaoai/lantai/internal/client"
)

func managementCommand(ctx context.Context, c *client.Client, action string, o options) (client.Response, error) {
	switch action {
	case "batch-create", "batch-revise":
		if o.output == "" {
			return client.Response{}, usage("batch preparation requires --output original-preparation.json")
		}
		var draft client.MigrationBatchDraft
		// A saved import plan hashes its original RawMessages. Generic YAML
		// conversion may reorder those objects and must not rewrite that input.
		if err := client.LoadState(o.input, &draft); err != nil {
			return client.Response{}, err
		}
		var previous *client.ManagementRecordRead
		if action == "batch-revise" {
			if o.directory == "" {
				return client.Response{}, usage("batch-revise requires an existing --directory for verified previous record bytes")
			}
			record, err := c.ReadManagementRecord(ctx, o.asset, o.version, o.directory)
			if err != nil {
				return client.Response{}, err
			}
			previous = &record
		}
		prepared, err := c.PrepareMigrationBatch(ctx, draft, previous)
		if err != nil {
			return client.Response{}, err
		}
		if err = client.WriteManagementPreparation(ctx, o.output, prepared); err != nil {
			return client.Response{}, err
		}
		return managementResponse(map[string]any{"schema": prepared.Schema, "prepared": o.output, "sha256": prepared.SHA256, "size": prepared.Size, "kind": "migration", "project_id": prepared.Spec.ProjectID, "complete": true})
	case "prepare":
		if o.output == "" {
			return client.Response{}, usage("management prepare requires --output private-preparation.json")
		}
		raw, err := input(o.input)
		if err != nil {
			return client.Response{}, err
		}
		var spec client.ManagementRecordSpec
		if err = strictDecode(raw, &spec); err != nil {
			return client.Response{}, err
		}
		prepared, err := c.PrepareManagementRecord(ctx, spec)
		if err != nil {
			return client.Response{}, err
		}
		if err = client.WriteManagementPreparation(ctx, o.output, prepared); err != nil {
			return client.Response{}, err
		}
		return managementResponse(map[string]any{"schema": prepared.Schema, "prepared": o.output, "sha256": prepared.SHA256, "size": prepared.Size, "kind": prepared.Spec.Kind, "project_id": prepared.Spec.ProjectID, "complete": true})
	case "save", "revise":
		if o.directory == "" || o.state == "" {
			return client.Response{}, usage("management save/revise requires --directory and original --state")
		}
		var prepared client.PreparedManagementRecord
		if err := client.LoadState(o.input, &prepared); err != nil {
			return client.Response{}, err
		}
		if action == "revise" && (prepared.Spec.AssetID == "" || prepared.Spec.BaseVersionID == "") {
			return client.Response{}, usage("management revise requires an explicitly prepared original asset/base version")
		}
		return c.SaveManagementRecord(ctx, prepared, o.directory, o.state)
	case "list":
		return c.ListManagementRecords(ctx, o.project, o.recordKind, o.cursor, o.limit)
	case "read":
		if o.directory == "" {
			return client.Response{}, usage("management read requires an existing --directory for verified record bytes")
		}
		record, err := c.ReadManagementRecord(ctx, o.asset, o.version, o.directory)
		if err != nil {
			return client.Response{}, err
		}
		return managementResponse(record)
	case "check":
		return c.CheckManagementBatch(ctx, o.asset, o.version)
	default:
		return client.Response{}, usage("management requires prepare, save, revise, list, read, batch-create, batch-revise or check")
	}
}

func managementResponse(value any) (client.Response, error) {
	raw, err := json.Marshal(value)
	return client.Response{Status: http.StatusOK, Body: raw}, err
}
