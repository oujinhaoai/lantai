package clock

import (
	"testing"
	"time"
)

func TestFormatAndParse(t *testing.T) {
	in := time.Date(2026, 9, 26, 18, 14, 34, 123_987_000, time.FixedZone("CST", 8*3600))
	s := Format(in)
	if s != "2026-09-26T10:14:34.123Z" {
		t.Fatalf("Format = %s", s)
	}
	back, err := Parse(s)
	if err != nil || !back.Equal(Truncate(in)) {
		t.Fatalf("Parse = %v, %v", back, err)
	}
	for _, bad := range []string{
		"2026-09-26T18:14:34.123+08:00",
		"2026-09-26T10:14:34Z",
		"2026-09-26T10:14:34.1234Z",
		"2026-02-30T10:14:34.123Z",
		"2026-09-26 10:14:34.123Z",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if Millis(back) != 1790417674123 || !FromMillis(1790417674123).Equal(back) {
		t.Fatalf("millis round trip: %d", Millis(back))
	}
}

func TestFake(t *testing.T) {
	f := NewFake(time.Date(2026, 1, 1, 0, 0, 0, 999_999, time.UTC))
	if f.Now().Nanosecond() != 0 {
		t.Fatal("fake clock must truncate to milliseconds")
	}
	f.Advance(3 * time.Hour)
	if got := f.Now(); got.Hour() != 3 {
		t.Fatalf("Advance: %v", got)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("moving time backwards must panic")
			}
		}()
		f.Set(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	}()
	if (System{}).Now().Location() != time.UTC {
		t.Fatal("system clock must be UTC")
	}
}
