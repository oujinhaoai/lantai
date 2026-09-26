package event

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

func testGen() (*ids.Generator, *clock.Fake) {
	clk := clock.NewFake(time.Date(2026, 9, 26, 10, 14, 34, 123_000_000, time.UTC))
	return &ids.Generator{Clock: clk, Rand: strings.NewReader(strings.Repeat("\x01", 1000))}, clk
}

func TestNewProducesValidCanonicalEnvelope(t *testing.T) {
	gen, clk := testGen()
	op := gen.MustNew()
	e, err := New(gen, clk, Params{
		EventType: "version.committed", SchemaVersion: 1,
		AggregateType: "asset", AggregateID: gen.MustNew(), AggregateRevision: 3,
		ActorID: gen.MustNew(), ProjectID: gen.MustNew(), OperationID: op,
		Payload: map[string]any{"number": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.CorrelationID != op {
		t.Fatal("correlation_id should default to operation_id")
	}
	c1, err := e.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(c1)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := back.Canonical()
	if string(c1) != string(c2) {
		t.Fatalf("round trip changed bytes:\n%s\n%s", c1, c2)
	}
	if !strings.Contains(string(c1), `"occurred_at":"2026-09-26T10:14:34.123Z"`) {
		t.Fatalf("timestamp format: %s", c1)
	}
}

func TestInvalidEnvelopesRejected(t *testing.T) {
	gen, clk := testGen()
	base := Params{
		EventType: "version.committed", SchemaVersion: 1,
		AggregateType: "asset", AggregateID: gen.MustNew(), AggregateRevision: 1,
		ActorID: gen.MustNew(), OperationID: gen.MustNew(),
	}
	for name, mutate := range map[string]func(*Params){
		"single segment type": func(p *Params) { p.EventType = "committed" },
		"zero revision":       func(p *Params) { p.AggregateRevision = 0 },
		"bad aggregate id":    func(p *Params) { p.AggregateID = "not-a-ulid" },
		"missing operation":   func(p *Params) { p.OperationID = "" },
		"array payload":       func(p *Params) { p.Payload = []int{1} },
		"zero schema version": func(p *Params) { p.SchemaVersion = 0 },
	} {
		p := base
		mutate(&p)
		if _, err := New(gen, clk, p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte(`{"event_id":"x"}`)); err == nil {
		t.Error("Parse accepted an incomplete envelope")
	}
}

// 事件正例经 Parse→Canonical 后与规范化原文一致，说明 Go 类型没有丢字段。
func TestExamplesRoundTrip(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "schemas", "examples", "common", "v1", "event-envelope")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := schema.Default()
	for _, ent := range entries {
		raw, _ := os.ReadFile(filepath.Join(dir, ent.Name()))
		var c struct {
			Valid    bool            `json:"valid"`
			Document json.RawMessage `json:"document"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if !c.Valid {
			if _, err := Parse(c.Document); err == nil {
				t.Errorf("%s: invalid example parsed", ent.Name())
			}
			continue
		}
		e, err := Parse(c.Document)
		if err != nil {
			t.Fatalf("%s: %v", ent.Name(), err)
		}
		got, _ := e.Canonical()
		want, _ := canonjson.Canonicalize(c.Document)
		if string(got) != string(want) {
			t.Errorf("%s: round trip mismatch\n%s\n%s", ent.Name(), got, want)
		}
		if err := reg.ValidateJSON(Contract, got); err != nil {
			t.Errorf("%s: %v", ent.Name(), err)
		}
	}
}

func TestStoreEntryJSON(t *testing.T) {
	reg, _ := schema.Default()
	data, _ := json.Marshal(StoreEntry{GlobalSeq: 7, RecordedAt: time.Unix(1, 0), EventID: ids.MustParse("01J8Z3K4M5N6P7Q8R9S0T1V2W3")})
	if err := reg.ValidateJSON("lantai.event-store-entry/v1", data); err != nil {
		t.Fatal(err)
	}
}
