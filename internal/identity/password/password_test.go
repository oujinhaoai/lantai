package password

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fast 是测试用的低成本参数；生产参数见 Default。
var fast = Params{Algorithm: "argon2id", Version: Default.Version, Memory: 64, Time: 1, Threads: 1, KeyLen: 32}

func TestHashVerify(t *testing.T) {
	h, err := NewHasher(fast, 2)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Hash(t.Context(), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := h.Verify(t.Context(), "correct horse battery", r)
	if err != nil || !ok {
		t.Fatalf("verify = %v %v", ok, err)
	}
	if ok, _ := h.Verify(t.Context(), "correct horse batterY", r); ok {
		t.Fatal("wrong password accepted")
	}
	r2, _ := h.Hash(t.Context(), "correct horse battery")
	if string(r2.Salt) == string(r.Salt) || string(r2.Hash) == string(r.Hash) {
		t.Fatal("salts must differ")
	}
	js, err := r.ParamsJSON()
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseParams(js)
	if err != nil || p != fast {
		t.Fatalf("params round trip = %+v %v", p, err)
	}
	// 记录按自身参数校验：换了 Hasher 默认参数，旧记录仍可验证。
	h2, _ := NewHasher(Params{Algorithm: "argon2id", Version: Default.Version, Memory: 128, Time: 2, Threads: 1, KeyLen: 32}, 1)
	if ok, _ := h2.Verify(t.Context(), "correct horse battery", r); !ok {
		t.Fatal("record must verify with its own params")
	}
}

func TestNFCNormalization(t *testing.T) {
	h, _ := NewHasher(fast, 1)
	nfc := "café-password-1"
	nfd := "café-password-1"
	r, err := h.Hash(t.Context(), nfd)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := h.Verify(t.Context(), nfc, r); !ok {
		t.Fatal("NFC and NFD input of the same password must match")
	}
}

func TestPolicy(t *testing.T) {
	h, _ := NewHasher(fast, 1)
	for _, bad := range []string{"short", strings.Repeat("x", MaxBytes+1), "invalid-\xff-utf8-pw"} {
		if _, err := h.Hash(t.Context(), bad); !errors.Is(err, ErrPolicy) {
			t.Errorf("%q: %v", bad[:5], err)
		}
	}
	for _, p := range []string{`{"alg":"bcrypt","v":19,"m":64,"t":1,"p":1,"len":32}`, `{"alg":"argon2id","v":19,"m":1,"t":1,"p":1,"len":32}`, `x`} {
		if _, err := ParseParams(p); err == nil {
			t.Errorf("params %s accepted", p)
		}
	}
	if _, err := NewHasher(Params{}, 1); err == nil {
		t.Fatal("zero params accepted")
	}
	if err := Default.check(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	h, _ := NewHasher(fast, 1)
	if err := h.sem.Acquire(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "correct horse battery"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hash while saturated = %v", err)
	}
	h.sem.Release(1)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Burn(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// BenchmarkDefault 测量生产参数的单次计算耗时，用于在目标机器上核对参数：
// go test -run '^$' -bench Default ./internal/identity/password/
func BenchmarkDefault(b *testing.B) {
	h, err := NewHasher(Default, 1)
	if err != nil {
		b.Fatal(err)
	}
	r, err := h.Hash(context.Background(), "correct horse battery")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if ok, _ := h.Verify(context.Background(), "correct horse battery", r); !ok {
			b.Fatal("verify failed")
		}
	}
}
