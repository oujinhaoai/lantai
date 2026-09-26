package errcode_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

func TestEveryCodeProducesSchemaValidEnvelope(t *testing.T) {
	reg, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	if len(errcode.All()) == 0 {
		t.Fatal("registry is empty")
	}
	for _, s := range errcode.All() {
		env := errcode.New(s.Code, "").WithHint("h").WithRetryAfter(1500 * time.Millisecond).
			WithDetails(errcode.PointerDetail("required", "/a/0", "missing")).Envelope("req-1")
		data, _ := json.Marshal(env)
		if err := reg.ValidateJSON("lantai.error/v1", data); err != nil {
			t.Errorf("%s: %v", s.Code, err)
		}
		if err := errcode.CheckBody(env.Error); err != nil {
			t.Errorf("%s: %v", s.Code, err)
		}
		if env.Error.Message != s.Summary {
			t.Errorf("%s: empty message should default to summary", s.Code)
		}
	}
}

func TestNewRejectsUnregisteredAndSuperseded(t *testing.T) {
	for _, c := range []errcode.Code{"NO_SUCH_CODE", "UNAUTHENTICATED", "MISSING_BLOBS", "LEASE_EXPIRED"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("errcode.New(%s) should panic", c)
				}
			}()
			errcode.New(c, "x")
		}()
		if _, ok := errcode.Lookup(c); ok {
			t.Errorf("errcode.Lookup(%s) should fail", c)
		}
	}
	if repl, ok := errcode.Superseded("MISSING_BLOBS"); !ok || len(repl) != 1 || repl[0] != errcode.BlobGrantRequired {
		t.Fatalf("errcode.Superseded(MISSING_BLOBS) = %v %v", repl, ok)
	}
}

func TestErrorBehaviour(t *testing.T) {
	cause := errors.New("disk said no: /private/secret/path")
	e := errcode.Wrap(errcode.StorageUnavailable, "存储暂不可写", cause).WithOperation("01J8Z3K4M5N6P7Q8R9S0T1V2W3")
	if !errors.Is(e, cause) {
		t.Fatal("cause should be reachable")
	}
	if !errors.Is(e, errcode.New(errcode.StorageUnavailable, "")) || errors.Is(e, errcode.New(errcode.StorageFull, "")) {
		t.Fatal("errors.Is should match by code")
	}
	if e.HTTPStatus() != 503 || !e.Retryable() || e.Recovery() != errcode.ActionRetry {
		t.Fatalf("spec = %+v", e.Spec())
	}
	body, _ := json.Marshal(e.Envelope(""))
	if strings.Contains(string(body), "secret") {
		t.Fatalf("envelope leaked the internal cause: %s", body)
	}
	wrapped := errcode.From(errors.Join(errors.New("ctx"), e))
	if wrapped.Code != errcode.StorageUnavailable {
		t.Fatalf("errcode.From should find the structured error, got %s", wrapped.Code)
	}
	plain := errcode.From(errors.New("boom at /Users/someone"))
	if plain.Code != errcode.Internal || strings.Contains(plain.Message, "/Users") {
		t.Fatalf("unstructured errors must become a generic INTERNAL, got %+v", plain)
	}
	if errcode.CodeOf(errors.New("x")) != errcode.Internal || errcode.From(nil) != nil {
		t.Fatal("CodeOf/errcode.From nil handling")
	}
	small := errcode.New(errcode.RateLimited, "slow down").WithRetryAfter(100 * time.Microsecond).Envelope("")
	if small.Error.RetryAfterMS == nil || *small.Error.RetryAfterMS != 1 {
		t.Fatal("sub-millisecond retry_after must round up to 1ms")
	}
}

func TestCheckBodyDetectsDrift(t *testing.T) {
	good := errcode.New(errcode.LeaseStale, "stale").Envelope("").Error
	if err := errcode.CheckBody(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*errcode.Body){
		"retryable":  func(b *errcode.Body) { b.Retryable = true },
		"recovery":   func(b *errcode.Body) { b.RecoveryAction = errcode.ActionRetry },
		"unknown":    func(b *errcode.Body) { b.Code = "NOPE" },
		"superseded": func(b *errcode.Body) { b.Code = "LEASE_EXPIRED" },
	} {
		b := good
		mutate(&b)
		if errcode.CheckBody(b) == nil {
			t.Errorf("%s drift not detected", name)
		}
	}
}

// 正例中的错误信封必须与登记一致，保证“同一错误在契约及示例中含义一致”。
func TestErrorExamplesMatchRegistry(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "schemas", "examples", "common", "v1", "error")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, ent := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Valid    bool             `json:"valid"`
			Document errcode.Envelope `json:"document"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", ent.Name(), err)
		}
		if !c.Valid {
			continue
		}
		if err := errcode.CheckBody(c.Document.Error); err != nil {
			t.Errorf("%s: %v", ent.Name(), err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no valid error examples found")
	}
}
