package managementrecord

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestSharedRecordCanonicalIdentityAndPrivatePayload(t *testing.T) {
	in := Envelope{Schema: Contract, Kind: "migration", InstanceID: ids.New(), ProjectID: ids.New(), Payload: json.RawMessage(`{"original":{"create_key":"saved-key","operation_id":"fixed-op"},"unknown":true}`)}
	raw, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil || got.InstanceID != in.InstanceID || got.ProjectID != in.ProjectID || got.Kind != in.Kind {
		t.Fatal(got, err)
	}
	again, err := Render(got)
	if err != nil || string(again) != string(raw) {
		t.Fatal("record is not reconstructable", err)
	}
	rules, kinds, err := Definition()
	if err != nil || len(kinds) != 4 {
		t.Fatal(rules, kinds, err)
	}
	for _, key := range rules.ForbiddenFields {
		payload := []byte(`{"nested":[{"` + strings.ToUpper(key) + `":"synthetic-secret"}]}`)
		if ValidatePayload(payload) == nil {
			t.Fatal("nested credential accepted", key)
		}
	}
	for _, payload := range []string{`{"duplicate":1,"duplicate":2}`, `{"bad":NaN}`, strings.Repeat("[", 66) + "null" + strings.Repeat("]", 66)} {
		if ValidatePayload([]byte(payload)) == nil {
			t.Fatal("ambiguous or unbounded payload accepted")
		}
	}
	in.Payload = json.RawMessage(`{"large":"` + strings.Repeat("x", rules.MaxRecordBytes) + `"}`)
	if _, err := Render(in); err == nil {
		t.Fatal("oversize record accepted")
	}
}

func TestRecordRejectsUnknownEnvelopeAndKind(t *testing.T) {
	in := Envelope{Schema: Contract, Kind: "workset", InstanceID: ids.New(), ProjectID: ids.New(), Payload: json.RawMessage(`{}`)}
	raw, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	fields["ignored_authorization"] = true
	unknown, _ := json.Marshal(fields)
	if _, err := Decode(unknown); err == nil {
		t.Fatal("unknown envelope field ignored")
	}
	in.Kind = "shell"
	if _, err := Render(in); err == nil {
		t.Fatal("undeclared record kind accepted")
	}
}
