package integration

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

func TestIntegrationFreshTOTPSkipsAdjacentCollision(t *testing.T) {
	// Synthetic seed, not a real credential: counters 59683440 and 59683441
	// both produce 312945. Bootstrap therefore consumes the larger counter.
	secret, err := hex.DecodeString("66e488030f5f6b12961a75cc321e8cf5aec12cc7")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
	counter := totp.Counter(clk.Now())
	if totp.Code(secret, counter) != totp.Code(secret, counter+1) {
		t.Fatal("synthetic adjacent collision was not reproduced")
	}
	result, last := totp.Verify(secret, totp.Code(secret, counter), clk.Now(), -1)
	if result != totp.Accepted || last != counter+1 {
		t.Fatalf("bootstrap collision = %v, counter %d", result, last)
	}
	e := &env{t: t, clk: clk, secret: secret}
	for i := 0; i < 10; i++ {
		code := e.fresh()
		result, accepted := totp.Verify(secret, code, clk.Now(), last)
		if result != totp.Accepted || accepted != totp.Counter(clk.Now()) || accepted <= last {
			t.Fatalf("fresh code %d = %v, counter %d, last %d", i, result, accepted, last)
		}
		if result, _ = totp.Verify(secret, code, clk.Now(), accepted); result != totp.Replayed {
			t.Fatalf("accepted code replay = %v", result)
		}
		last = accepted
	}
}

func TestIntegrationFreshTOTPAdvancesOneUnambiguousStep(t *testing.T) {
	start := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	secret := []byte("12345678901234567890")
	e := &env{t: t, clk: clk, secret: secret}
	code := e.fresh()
	if !clk.Now().Equal(start.Add(totp.Period * time.Second)) {
		t.Fatal("ordinary fresh code changed the fixture time advance")
	}
	if result, accepted := totp.Verify(secret, code, clk.Now(), totp.Counter(start)); result != totp.Accepted || accepted != totp.Counter(clk.Now()) {
		t.Fatalf("ordinary fresh code = %v, counter %d", result, accepted)
	}
}

func TestIntegrationFreshTOTPSkipsFutureCollision(t *testing.T) {
	secret, err := hex.DecodeString("66e488030f5f6b12961a75cc321e8cf5aec12cc7")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 9, 27, 9, 59, 30, 0, time.UTC))
	previous := totp.Counter(clk.Now())
	e := &env{t: t, clk: clk, secret: secret}
	code := e.fresh()
	result, accepted := totp.Verify(secret, code, clk.Now(), previous)
	if result != totp.Accepted || accepted != totp.Counter(clk.Now()) {
		t.Fatalf("future-window collision = %v, counter %d", result, accepted)
	}
	if clk.Now().Before(time.Date(2026, 9, 27, 10, 1, 0, 0, time.UTC)) {
		t.Fatal("fresh code did not skip both colliding counters")
	}
}
