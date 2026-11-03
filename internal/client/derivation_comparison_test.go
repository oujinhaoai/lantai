package client

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestDerivationComparisonSharedPageSemantics(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/asset-derivations/comparison-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string      `json:"name"`
		Before   []InputFile `json:"before"`
		After    []InputFile `json:"after"`
		Expected []string    `json:"expected"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			out := compareDerivationManifests(derivationManifest{files: tc.Before, content: map[string]json.RawMessage{"metadata": json.RawMessage(`{"b":2,"a":1}`)}}, derivationManifest{files: tc.After, content: map[string]json.RawMessage{"metadata": json.RawMessage(`{"a":1,"b":2}`)}})
			var kinds []string
			for _, row := range out.Files {
				kinds = append(kinds, row.Kind)
			}
			if !reflect.DeepEqual(kinds, tc.Expected) {
				t.Fatal(kinds, tc.Expected)
			}
			for _, field := range out.Fields {
				if field.Changed {
					t.Fatal("metadata ordering became content change")
				}
			}
		})
	}
}
