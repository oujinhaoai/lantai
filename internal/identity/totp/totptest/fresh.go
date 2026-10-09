// Package totptest provides TOTP codes for tests using a controlled clock.
package totptest

import (
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

// Fresh advances at least one step and returns a code unique within the
// verification window. Adjacent six-digit codes can collide; Verify correctly
// consumes the largest matching counter, so advancing one step alone can replay
// a counter already consumed by bootstrap or a previous login. Only the fake
// clock advances here; callers still use the real login/challenge validation.
func Fresh(t testing.TB, clk *clock.Fake, secret []byte) string {
	t.Helper()
	for range 10 {
		now := clk.Advance(totp.Period * time.Second)
		counter := totp.Counter(now)
		code := totp.Code(secret, counter)
		unique := true
		for offset := int64(-totp.Skew); offset <= totp.Skew; offset++ {
			if offset != 0 && totp.Code(secret, counter+offset) == code {
				unique = false
				break
			}
		}
		if unique {
			return code
		}
	}
	t.Fatal("cannot find an unambiguous test TOTP code within 10 steps")
	return ""
}
