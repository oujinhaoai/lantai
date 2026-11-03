package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/oujinhaoai/lantai/internal/client"
)

func executeManagedWorkset(ctx context.Context, name string, o assetOptions) (any, error) {
	if name != "workset-list" && o.output == "" {
		return nil, usage("managed workset actions require --output")
	}
	var filter struct {
		ProjectID string `json:"project_id"`
		Cursor    string `json:"cursor,omitempty"`
		Limit     int    `json:"limit,omitempty"`
	}
	var ref struct {
		AssetID   string `json:"asset_id"`
		VersionID string `json:"version_id"`
	}
	var record client.WorksetRecordSpec
	var prepared client.PreparedManagementRecord
	var spec client.WorksetReturnSpec
	var returning client.WorksetReturn
	var adoption client.WorksetAdoption
	var value any
	switch name {
	case "workset-list":
		value = &filter
	case "workset-open":
		value = &ref
	case "workset-record-prepare":
		value = &record
	case "workset-record-save":
		value = &prepared
	case "workset-return-prepare":
		value = &spec
	case "workset-return-apply":
		value = &returning
	case "workset-adopt-prepare":
		value = &adoption
	}
	if err := decodeAssetInput(o.input, value); err != nil {
		return nil, err
	}
	if name == "workset-record-save" && prepared.Spec.Kind != "workset" {
		return nil, usage("selected preparation must be a workset record")
	}
	if (name == "workset-record-save" || name == "workset-return-apply") && o.state == "" {
		return nil, usage("original --state is required")
	}
	c, err := assetClient(o)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	decode := func(response client.Response, err error) (any, error) {
		if len(response.Body) == 0 {
			return nil, err
		}
		return json.RawMessage(response.Body), err
	}
	switch name {
	case "workset-list":
		if filter.Limit == 0 {
			filter.Limit = 10
		}
		if filter.Limit < 1 || filter.Limit > 20 {
			return nil, usage("workset list limit must be1..20")
		}
		return decode(c.ListManagementRecords(ctx, filter.ProjectID, "workset", filter.Cursor, filter.Limit))
	case "workset-open":
		r, stored, err := c.ReadWorksetRecord(ctx, ref.AssetID, ref.VersionID, o.directory)
		if err != nil {
			return nil, err
		}
		return map[string]any{"asset_id": r.AssetID, "version_id": r.VersionID, "record_sha256": r.SHA256, "record_bytes": r.Size, "payload": stored, "verification_scope": "record_bytes_and_current_fixed_manifest_metadata"}, nil
	case "workset-record-prepare":
		return c.PrepareWorksetRecord(ctx, record)
	case "workset-record-save":
		return decode(c.SaveManagementRecord(ctx, prepared, o.directory, o.state))
	case "workset-return-prepare":
		var original *client.WorksetReturn
		path := o.report
		if path == "" {
			path = o.output
		}
		if _, err := os.Stat(path); err == nil {
			var prior client.WorksetReturn
			if err := decodeAssetInput(path, &prior); err != nil {
				return nil, err
			}
			original = &prior
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return c.PrepareWorksetReturn(ctx, spec, o.directory, original)
	case "workset-return-apply":
		return decode(c.ApplyWorksetReturn(ctx, returning, o.directory, o.state))
	case "workset-adopt-prepare":
		return c.PrepareWorksetAdoption(ctx, adoption, o.directory)
	}
	return nil, usage("unknown managed workset action")
}
