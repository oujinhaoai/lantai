package workflow

import (
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func TestBuiltinDefinitionsValidateAndPinDigest(t *testing.T) {
	for _, kind := range []string{"ingest", "create", "modify"} {
		d, err := BuiltinDefinition(kind)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(d)
		var doc any
		if err = json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		parsed, sum, err := ParseDefinition(doc)
		if err != nil || parsed.Kind != kind || !sum.Valid() {
			t.Fatal(kind, err)
		}
		_, again, _ := ParseDefinition(doc)
		if again != sum {
			t.Fatal("digest must be canonical and stable")
		}
	}
	if _, err := BuiltinDefinition("parallel"); err == nil {
		t.Fatal("unknown built-in kind accepted")
	}
}

func TestDefinitionRejectsNonFixedShapes(t *testing.T) {
	base, _ := BuiltinDefinition("create")
	cases := map[string]func(d *Definition){
		"review first": func(d *Definition) { d.Steps[0], d.Steps[3] = d.Steps[3], d.Steps[0] },
		"qa not independent": func(d *Definition) {
			d.Steps[2].Task.DistinctFrom = "check"
		},
		"publish before review": func(d *Definition) { d.Steps[3], d.Steps[4] = d.Steps[4], d.Steps[3] },
		"missing review":        func(d *Definition) { d.Steps = append(d.Steps[:3], d.Steps[4]) },
		"duplicate key":         func(d *Definition) { d.Steps[1].Key = "produce" },
		"modify without checkout": func(d *Definition) {
			d.Kind = "modify"
			d.Steps[0].Task.Checkout = "none"
		},
		"qa produces first": func(d *Definition) { d.Steps[0].Task.Type = "qa" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var d Definition
			_ = json.Unmarshal(raw, &d)
			mutate(&d)
			raw, _ = json.Marshal(d)
			var doc any
			_ = json.Unmarshal(raw, &doc)
			_, _, err := ParseDefinition(doc)
			if code := errcode.CodeOf(err); code != errcode.UnsupportedCapability && code != errcode.SchemaInvalid {
				t.Fatalf("want rejection, got %v", err)
			}
		})
	}
	var doc any
	_ = json.Unmarshal([]byte(`{"contract":"lantai.flow-definition/v1","key":"x","version":1,"kind":"create","steps":[{"key":"a","kind":"task","task":{"type":"produce"}},{"key":"b","kind":"foreach"},{"key":"r","kind":"review"},{"key":"p","kind":"publish"}]}`), &doc)
	if _, _, err := ParseDefinition(doc); errcode.CodeOf(err) != errcode.SchemaInvalid {
		t.Fatal("unsupported step kinds are rejected by the schema", err)
	}
}
