package ids

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
)

func TestEncodeKnownVectors(t *testing.T) {
	var zero [16]byte
	if got := encode(zero); got != "00000000000000000000000000" {
		t.Fatalf("zero = %s", got)
	}
	var max [16]byte
	for i := range max {
		max[i] = 0xFF
	}
	if got := encode(max); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("max = %s", got)
	}
	// ULID 规范示例 01ARYZ6S41TSV4RRFFQ69G5FAV 的时间成分是 1469918176385 ms。
	g := &Generator{Clock: clock.NewFake(time.UnixMilli(1469918176385)), Rand: bytes.NewReader(make([]byte, 10))}
	id := g.MustNew()
	if !strings.HasPrefix(string(id), "01ARYZ6S41") {
		t.Fatalf("time prefix = %s", id)
	}
	ts, err := id.Time()
	if err != nil || ts.UnixMilli() != 1469918176385 {
		t.Fatalf("Time() = %v, %v", ts, err)
	}
}

func TestRoundTrip(t *testing.T) {
	for range 200 {
		id := New()
		b, err := decode(string(id))
		if err != nil {
			t.Fatalf("decode(%s): %v", id, err)
		}
		if encode(b) != string(id) {
			t.Fatalf("round trip mismatch for %s", id)
		}
	}
}

func TestParseRejectsNonCanonical(t *testing.T) {
	valid := "01ARYZ6S41TSV4RRFFQ69G5FAV"
	if _, err := Parse(valid); err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
	for _, s := range []string{
		"",
		strings.ToLower(valid),        // 小写不是规范形式
		"01ARYZ6S41TSV4RRFFQ69G5FA",   // 25 位
		"01ARYZ6S41TSV4RRFFQ69G5FAVX", // 27 位
		"81ARYZ6S41TSV4RRFFQ69G5FAV",  // 首位超过 7 会溢出 128 位
		"01ARYZ6S41TSV4RRFFQ69G5FAI",  // I 不在字母表
		"01ARYZ6S41TSV4RRFFQ69G5FAL",
		"01ARYZ6S41TSV4RRFFQ69G5FAO",
		"01ARYZ6S41TSV4RRFFQ69G5FAU",
		"01ARYZ6S41TSV4RRFFQ69G5FA-",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

func TestGeneratorRejectsOutOfRangeClock(t *testing.T) {
	g := &Generator{Clock: clock.NewFake(time.UnixMilli(maxTime + 1)), Rand: bytes.NewReader(make([]byte, 10))}
	if _, err := g.New(); err == nil {
		t.Fatal("expected error for clock beyond 48-bit range")
	}
}

func TestDeriveChildIsStableAndDistinct(t *testing.T) {
	parent := MustParse("01ARYZ6S41TSV4RRFFQ69G5FAV")
	a1, err := DeriveChild(parent, "item:01ARYZ6S41TSV4RRFFQ69G5FAW")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := DeriveChild(parent, "item:01ARYZ6S41TSV4RRFFQ69G5FAW")
	b, _ := DeriveChild(parent, "item:01ARYZ6S41TSV4RRFFQ69G5FAX")
	if a1 != a2 {
		t.Fatalf("derivation not stable: %s != %s", a1, a2)
	}
	if a1 == b || a1 == parent {
		t.Fatalf("derived ids collide: %s %s %s", a1, b, parent)
	}
	if !a1.Valid() {
		t.Fatalf("derived id not canonical: %s", a1)
	}
	if a1[:10] != parent[:10] {
		t.Fatalf("derived id should keep parent time prefix: %s vs %s", a1, parent)
	}
	other := MustParse("01ARYZ6S41TSV4RRFFQ69G5FAW")
	c, _ := DeriveChild(other, "item:01ARYZ6S41TSV4RRFFQ69G5FAW")
	if c == a1 {
		t.Fatal("different parents must derive different children")
	}
	for _, bad := range []string{"", "has space", strings.Repeat("x", 201), "中文"} {
		if _, err := DeriveChild(parent, bad); err == nil {
			t.Errorf("DeriveChild accepted step %q", bad)
		}
	}
	if _, err := DeriveChild("bad", "step:x"); err == nil {
		t.Error("DeriveChild accepted invalid parent")
	}
}

func TestPermanentRefURI(t *testing.T) {
	ref := PermanentRef{
		InstanceID: MustParse("01J0000000000000000000000A"),
		AssetID:    MustParse("01J0000000000000000000000B"),
		VersionID:  MustParse("01J0000000000000000000000C"),
	}
	uri, err := ref.URI()
	if err != nil {
		t.Fatal(err)
	}
	want := "lantai://01J0000000000000000000000A/assets/01J0000000000000000000000B/versions/01J0000000000000000000000C"
	if uri != want {
		t.Fatalf("URI = %s", uri)
	}
	back, err := ParseURI(uri)
	if err != nil || back != ref {
		t.Fatalf("ParseURI = %+v, %v", back, err)
	}
	local := PermanentRef{AssetID: ref.AssetID, VersionID: ref.VersionID}
	if err := local.Validate(false); err != nil {
		t.Fatalf("local ref should validate without instance: %v", err)
	}
	if err := local.Validate(true); err == nil {
		t.Fatal("persisted ref must require instance_id")
	}
	if _, err := local.URI(); err == nil {
		t.Fatal("URI requires instance_id")
	}
	if got := local.Complete(ref.InstanceID); got != ref {
		t.Fatalf("Complete = %+v", got)
	}
	for _, bad := range []string{
		"https://01J0000000000000000000000A/assets/01J0000000000000000000000B/versions/01J0000000000000000000000C",
		"lantai://01J0000000000000000000000A/assets/01J0000000000000000000000B",
		"lantai://01J0000000000000000000000A/versions/01J0000000000000000000000B/assets/01J0000000000000000000000C",
		"lantai://01j0000000000000000000000a/assets/01J0000000000000000000000B/versions/01J0000000000000000000000C",
		"lantai://01J0000000000000000000000A/assets/01J0000000000000000000000B/versions/01J0000000000000000000000C/",
	} {
		if _, err := ParseURI(bad); err == nil {
			t.Errorf("ParseURI accepted %q", bad)
		}
	}
}
