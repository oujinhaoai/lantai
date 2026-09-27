package schema

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/schemas"
)

func TestIndexCoversEverySchemaFile(t *testing.T) {
	r := mustDefault(t)
	indexed := map[string]bool{}
	for _, e := range r.Entries() {
		indexed[e.Path] = true
	}
	err := fs.WalkDir(schemas.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".schema.json") && !indexed[p] {
			t.Errorf("%s is not listed in schemas/index.json", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLookupAndLibraryGuard(t *testing.T) {
	r := mustDefault(t)
	e, ok := r.Lookup("lantai.error/v1")
	if !ok || e.URI() != schemas.BaseURI+"common/v1/error.schema.json" {
		t.Fatalf("Lookup = %+v %v", e, ok)
	}
	if _, ok := r.Lookup(e.URI()); !ok {
		t.Fatal("lookup by $id failed")
	}
	if err := r.Validate("lantai.common-defs/v1", "x"); err == nil {
		t.Fatal("validating against a library without fragment must fail")
	}
	if err := r.Validate("lantai.nope/v1", map[string]any{}); !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("unknown contract error = %v", err)
	}
	for _, key := range []string{"lantai.common-defs/v1#", "lantai.common-defs/v1#/$defs/nope", "lantai.error/v1#x", "lantai.common-defs/v1"} {
		if err := r.Check(key); !errors.Is(err, ErrUnknownSchema) {
			t.Errorf("Check(%q) = %v", key, err)
		}
	}
	if err := r.Check("lantai.common-defs/v1#/$defs/ulid"); err != nil {
		t.Fatal(err)
	}
}

func TestValidationErrorShape(t *testing.T) {
	r := mustDefault(t)
	err := r.ValidateJSON("lantai.error/v1", []byte(`{"error":{"code":"x","message":"m","retryable":"no"}}`))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	var got []string
	for _, is := range ve.Issues {
		got = append(got, is.Pointer+" "+is.Keyword)
	}
	joined := strings.Join(got, ",")
	for _, want := range []string{"/error/code pattern", "/error/retryable type", "/error required"} {
		if !strings.Contains(joined, want) {
			t.Errorf("issues %q missing %q", joined, want)
		}
	}
}

func TestValidationErrorAsSchemaInvalid(t *testing.T) {
	r := mustDefault(t)
	err := r.ValidateJSON("lantai.error/v1", []byte(`{"error":{"code":"X","message":"m","retryable":false,"recovery_action":"none","extra":1}}`))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatal(err)
	}
	e := ve.Err()
	if e.Code != errcode.SchemaInvalid || len(e.Details) == 0 {
		t.Fatalf("Err() = %+v", e)
	}
	env, _ := json.Marshal(e.Envelope("req"))
	if err := r.ValidateJSON("lantai.error/v1", env); err != nil {
		t.Fatalf("SCHEMA_INVALID envelope must itself be valid: %v\n%s", err, env)
	}
	if e.Details[0].Reason != "additional_properties" {
		t.Fatalf("reason = %s", e.Details[0].Reason)
	}
}

func TestYAMLInput(t *testing.T) {
	r := mustDefault(t)
	y := "error:\n  code: RATE_LIMITED\n  message: 请求过快\n  retryable: true\n  recovery_action: retry\n  retry_after_ms: 1000\n"
	if err := r.ValidateYAML("lantai.error/v1", []byte(y)); err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateYAML("lantai.error/v1", []byte("error: &a {}\nx: *a\n")); err == nil {
		t.Fatal("anchors must be rejected before schema validation")
	}
}

func TestExtremeNumbersAreRejectedBeforeValidation(t *testing.T) {
	r := mustDefault(t)
	for _, literal := range []string{"1e-100000000", "0e-100000000", "1e-99999999999999999999999999999", "1e-1025"} {
		for _, format := range []string{"json", "yaml"} {
			t.Run(format+"/"+literal, func(t *testing.T) {
				// 21 个元素会走 uniqueItems 哈希分支；极端指数不得到达
				// big.Rat 的指数展开，即便浮点解码把它下溢成 0。
				items := literal + ",0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19"
				var err error
				if format == "json" {
					err = r.ValidateJSON("lantai.pin/v1", []byte(`{"blobs":[`+items+`]}`))
				} else {
					err = r.ValidateYAML("lantai.pin/v1", []byte("blobs: ["+items+"]\n"))
				}
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Err().Code != errcode.SchemaInvalid || ve.Issues[0].Pointer != "/blobs/0" {
					t.Fatalf("want bounded SCHEMA_INVALID at the number, got %v", err)
				}
			})
		}
	}
	for _, value := range []any{json.Number("1e100000000"), json.Number("1e-100000000"),
		json.Number(strings.Repeat("1", maxNumberLiteralBytes+1)), json.Number("1 "), json.Number("NaN"),
		math.Inf(1), math.NaN(), float32(math.Inf(-1))} {
		err := r.Validate("lantai.common-defs/v1#/$defs/expected_revision", value)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Issues[0].Keyword != "number" {
			t.Fatalf("direct Validate(%v) bypassed the number guard: %v", value, err)
		}
	}
}

func TestNumberGuardPreservesExactSchemaArithmetic(t *testing.T) {
	r := mustDefault(t)
	for _, test := range []struct {
		literal string
		valid   bool
	}{
		{"9007199254740991", true},
		{"9007199254740991.0", true},
		{"9007199254740992", false},
		{"9007199254740991.1", false}, // 不能舍入到合法整数。
		{"1.0000000000000001", false},
		{"1e-400", false}, // 不能将原始非零小数当作浮点下溢后的整数 0。
		{"0e-400", true},
	} {
		t.Run(test.literal, func(t *testing.T) {
			err := r.Validate("lantai.common-defs/v1#/$defs/expected_revision", json.Number(test.literal))
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v: %v", test.valid, err)
			}
		})
	}
}

// exampleCase 是 schemas/examples 下的正反例文件格式。
type exampleCase struct {
	Description string          `json:"description"`
	Schema      string          `json:"schema"`
	Valid       *bool           `json:"valid"`
	Expect      []Issue         `json:"expect"`
	Document    json.RawMessage `json:"document"`
}

func TestExamples(t *testing.T) {
	r := mustDefault(t)
	root := filepath.Join("..", "..", "..", "schemas", "examples")
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		n++
		rel, _ := filepath.Rel(root, p)
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var c exampleCase
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil {
				t.Fatalf("case file: %v", err)
			}
			if c.Description == "" || c.Schema == "" || c.Valid == nil || len(c.Document) == 0 {
				t.Fatal("case needs description, schema, valid and document")
			}
			doc, err := canonjson.Decode(c.Document)
			if err != nil {
				t.Fatalf("document: %v", err)
			}
			err = r.Validate(c.Schema, doc)
			if *c.Valid {
				if err != nil {
					t.Fatalf("expected valid: %v", err)
				}
				if len(c.Expect) > 0 {
					t.Fatal("valid cases must not list expected issues")
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected validation failure, got %v", err)
			}
			if len(c.Expect) == 0 {
				t.Fatal("invalid cases must list at least one expected issue")
			}
			for _, want := range c.Expect {
				found := false
				for _, is := range ve.Issues {
					if is.Pointer == want.Pointer && is.Keyword == want.Keyword {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing issue %s %s; got %+v", want.Pointer, want.Keyword, ve.Issues)
				}
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no example cases found")
	}
}

func mustDefault(t *testing.T) *Registry {
	t.Helper()
	r, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	return r
}
