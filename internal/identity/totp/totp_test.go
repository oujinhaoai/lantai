package totp

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的 SHA1 测试向量（8 位码的末 6 位即 6 位码）。
func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	cases := []struct {
		unix int64
		code string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, c := range cases {
		if got := Code(secret, Counter(time.Unix(c.unix, 0))); got != c.code {
			t.Errorf("T=%d: %s, want %s", c.unix, got, c.code)
		}
	}
}

func TestVerifyWindowAndReplay(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111111, 0)
	cur := Counter(now)
	for _, d := range []int64{-1, 0, 1} {
		res, c := Verify(secret, Code(secret, cur+d), now, -1)
		if res != Accepted || c != cur+d {
			t.Fatalf("drift %d: %v %d", d, res, c)
		}
	}
	if res, _ := Verify(secret, Code(secret, cur+2), now, -1); res != Invalid {
		t.Fatal("two steps ahead must be rejected")
	}
	if res, _ := Verify(secret, Code(secret, cur-2), now, -1); res != Invalid {
		t.Fatal("two steps behind must be rejected")
	}
	// 同一时间步成功后不能再用；更早的时间步也不行。
	res, c := Verify(secret, Code(secret, cur), now, -1)
	if res != Accepted {
		t.Fatal(res)
	}
	if res, _ := Verify(secret, Code(secret, cur), now, c); res != Replayed {
		t.Fatalf("reuse = %v", res)
	}
	if res, _ := Verify(secret, Code(secret, cur-1), now, c); res != Replayed {
		t.Fatalf("older step after success = %v", res)
	}
	if res, c2 := Verify(secret, Code(secret, cur+1), now, c); res != Accepted || c2 != cur+1 {
		t.Fatalf("next step = %v %d", res, c2)
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if res, _ := Verify(secret, bad, now, -1); res != Invalid {
			t.Errorf("%q accepted", bad)
		}
	}
	if res, _ := Verify(secret, " "+Code(secret, cur)[:3]+"-"+Code(secret, cur)[3:], now, -1); res != Accepted {
		t.Fatal("spaces and hyphens must be ignored")
	}
}

func TestURIAndSecret(t *testing.T) {
	s, err := NewSecret(nil)
	if err != nil || len(s) != SecretLen {
		t.Fatal(err)
	}
	u, err := url.Parse(URI("lantai", "ada", s))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || q.Get("secret") != EncodeSecret(s) || q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Fatalf("uri = %s", u)
	}
	if strings.Contains(EncodeSecret(s), "=") {
		t.Fatal("secret must not be padded")
	}
}
