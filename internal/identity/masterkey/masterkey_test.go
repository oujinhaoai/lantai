package masterkey

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

func newKey(t *testing.T) *Key {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
	k, err := Generate(&ids.Generator{Clock: clk, Rand: bytes.NewReader(bytes.Repeat([]byte{7}, 64))}, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpenBindsAAD(t *testing.T) {
	k := newKey(t)
	ct, err := k.Seal([]byte("totp seed"), []byte("factor:A"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("totp seed")) {
		t.Fatal("plaintext visible in ciphertext")
	}
	pt, err := k.Open(ct, []byte("factor:A"))
	if err != nil || string(pt) != "totp seed" {
		t.Fatalf("open = %q, %v", pt, err)
	}
	// 挪用到别的因子、改动密文、换密钥都打不开。
	if _, err := k.Open(ct, []byte("factor:B")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong aad = %v", err)
	}
	bad := bytes.Clone(ct)
	bad[len(bad)-1] ^= 1
	if _, err := k.Open(bad, []byte("factor:A")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered = %v", err)
	}
	other := newKey(t)
	other.material = bytes.Repeat([]byte{9}, keyLen)
	if _, err := other.Open(ct, []byte("factor:A")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("other key = %v", err)
	}
	if _, err := k.Open(ct[:5], nil); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("short = %v", err)
	}
	ct2, _ := k.Seal([]byte("totp seed"), []byte("factor:A"))
	if bytes.Equal(ct, ct2) {
		t.Fatal("nonces must differ")
	}
}

func TestSaveLoadNeverOverwrites(t *testing.T) {
	dir := t.TempDir() + "/secrets"
	if _, err := Load(dir); !errors.Is(err, ErrMissing) {
		t.Fatalf("load missing = %v", err)
	}
	k := newKey(t)
	if err := Save(dir, k); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, newKey(t)); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second save = %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != k.ID() || !bytes.Equal(got.material, k.material) {
		t.Fatal("round trip mismatch")
	}
	a, _ := k.Derive("csrf/v1")
	b, _ := got.Derive("csrf/v1")
	c, _ := got.Derive("other/v1")
	if !bytes.Equal(a, b) || bytes.Equal(a, c) || len(a) != 32 {
		t.Fatal("derivation must be deterministic and label-separated")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(Path(dir), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); !errors.Is(err, fsutil.ErrInsecurePermissions) {
			t.Fatalf("load world-readable key = %v", err)
		}
	}
}

func TestLoadRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{"{", `{"contract":"x"}`, `{"contract":"lantai.master-key/v1","key_id":"01J8Z3K4M5N6P7Q8R9S0T100A1","key":"c2hvcnQ=","created_at":"2026-09-27T00:00:00.000Z"}`} {
		os.WriteFile(Path(dir), []byte(body), 0o600)
		if _, err := Load(dir); err == nil || strings.Contains(err.Error(), "c2hvcnQ") {
			t.Fatalf("malformed key accepted or leaked: %v", err)
		}
	}
}
