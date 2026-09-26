package schema

import (
	"encoding/json"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/oujinhaoai/lantai/schemas"
)

// 领域模块（T05/T06/T09）的 schema 放在 schemas/<domain>/v1/，以相对 $ref 引用
// 公共定义；登记到 index.json 后即可与公共 schema 一起编译与校验。
func TestDomainSchemaCanReferenceCommonDefinitions(t *testing.T) {
	fsys := fstest.MapFS{}
	err := fs.WalkDir(schemas.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(schemas.FS, p)
		fsys[p] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys["tasks/v1/attempt-result.schema.json"] = &fstest.MapFile{Data: []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://github.com/oujinhaoai/lantai/raw/main/schemas/tasks/v1/attempt-result.schema.json",
  "title": "AttemptResultSample",
  "type": "object",
  "additionalProperties": false,
  "required": ["fence", "state", "occurred_at"],
  "properties": {
    "fence": { "$ref": "../../common/v1/execution.schema.json#/$defs/task_fence" },
    "activation": { "$ref": "../../common/v1/execution.schema.json#/$defs/activation_ref" },
    "state": { "$ref": "../../common/v1/execution.schema.json#/$defs/task_run_state" },
    "occurred_at": { "$ref": "../../common/v1/defs.schema.json#/$defs/timestamp" }
  }
}`)}
	raw, _ := fs.ReadFile(schemas.FS, "index.json")
	var idx index
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	idx.Schemas = append(idx.Schemas, Entry{Contract: "lantai.attempt-result-sample/v1",
		Path: "tasks/v1/attempt-result.schema.json", Kind: KindDocument, Owner: "T05"})
	patched, _ := json.Marshal(idx)
	fsys["index.json"] = &fstest.MapFile{Data: patched}
	r, err := Load(fsys)
	if err != nil {
		t.Fatalf("domain schema with common $refs must compile: %v", err)
	}
	ok := `{"fence":{"attempt_id":"01J8Z3K4M5N6P7Q8R9S0T1V2W3","lease_fence":3,"recovery_epoch":1},"state":"running","occurred_at":"2026-09-26T10:00:00.000Z"}`
	if err := r.ValidateJSON("lantai.attempt-result-sample/v1", []byte(ok)); err != nil {
		t.Fatal(err)
	}
	stale := `{"fence":{"attempt_id":"01J8Z3K4M5N6P7Q8R9S0T1V2W3","lease_fence":3},"state":"ready","occurred_at":"2026-09-26T10:00:00.000Z"}`
	err = r.ValidateJSON("lantai.attempt-result-sample/v1", []byte(stale))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected validation failure, got %v", err)
	}
	found := map[string]bool{}
	for _, is := range ve.Issues {
		found[is.Pointer+" "+is.Keyword] = true
	}
	if !found["/fence required"] || !found["/state enum"] {
		t.Fatalf("issues = %+v", ve.Issues)
	}
}
