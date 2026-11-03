package client

import (
	"encoding/json"
	"os"
	"testing"
)

func TestWorksetReturnSharedBrowserAndCLIState(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/workset-return/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string        `json:"name"`
		Return WorksetReturn `json:"return"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			p := tc.Return
			if err := validateReturnState(p.Input, p.State, p.State.Origin); err != nil {
				t.Fatal("cross-client original lexical state changed", err)
			}
			changed := p.State
			changed.CommitKey = changed.CreateKey
			if err := validateReturnState(p.Input, changed, p.State.Origin); err == nil {
				t.Fatal("equal original keys accepted")
			}
			changed = p.State
			changed.UploadID = "01J00000000000000000000007"
			if err := validateReturnState(p.Input, changed, p.State.Origin); err == nil {
				t.Fatal("half original upload identity accepted")
			}
			p.Input.Content.VersionNote = "changed"
			if err := validateReturnState(p.Input, p.State, p.State.Origin); err == nil {
				t.Fatal("changed input accepted with original hash")
			}
		})
	}
}
