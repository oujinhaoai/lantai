package integration

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestM2BusinessPublicContractPreservesActualRun(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	v, flow := f.candidate("docs/contract", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, nil)
	j := f.checkCandidate(v, flow, "org.lantai.corecheck.manifest")
	e, err := f.source.Evidence(ctx, f.owner, j.Evidence[0].EvidenceID)
	if err != nil {
		t.Fatal(err)
	}
	target := f.submitChecked(v, flow, base.Ref, j, false)
	cases := []struct {
		name        string
		source      any
		destination func() any
	}{
		{"job_check_input", j.Checks[0], func() any { return &apiv1.M2LedgerReviewEvidenceInput{} }},
		{"accepted_evidence", e, func() any { return &apiv1.M2LedgerAcceptedEvidence{} }},
		{"review_target", target, func() any { return &apiv1.M2LedgerReviewTarget{} }},
	}
	for _, c := range cases {
		t.Run(c.name+"_legacy_control", func(t *testing.T) {
			b, err := json.Marshal(c.source)
			if err != nil {
				t.Fatal(err)
			}
			var doc any
			if err = json.Unmarshal(b, &doc); err != nil {
				t.Fatal(err)
			}
			removeContractRun(doc)
			b, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err = dec.Decode(c.destination()); err != nil {
				t.Fatal("legacy control rejected", err)
			}
		})
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.source)
			if err != nil {
				t.Fatal(err)
			}
			captureGovernanceEvidence(t, "actual-domain-output.json", b)
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			destination := c.destination()
			if err = dec.Decode(destination); err != nil {
				t.Fatalf("public generated model rejects real core output: %v", err)
			}
			round, err := json.Marshal(destination)
			if err != nil {
				t.Fatal(err)
			}
			var original, decoded any
			if err = json.Unmarshal(b, &original); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(round, &decoded); err != nil {
				t.Fatal(err)
			}
			want, got := map[string]any{}, map[string]any{}
			contractRunFields(original, "", want)
			contractRunFields(decoded, "", got)
			if len(want) == 0 || !reflect.DeepEqual(want, got) {
				t.Fatal("public generated model dropped exact JobAttempt provenance")
			}
		})
	}
}

func contractRunFields(v any, path string, out map[string]any) {
	switch x := v.(type) {
	case map[string]any:
		for key, value := range x {
			if key == "check_run_id" {
				out[path+"/"+key] = value
			}
			contractRunFields(value, path+"/"+key, out)
		}
	case []any:
		for i, value := range x {
			contractRunFields(value, path+"/"+strconv.Itoa(i), out)
		}
	}
}

func removeContractRun(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "check_run_id")
		for _, v := range x {
			removeContractRun(v)
		}
	case []any:
		for _, v := range x {
			removeContractRun(v)
		}
	}
}
