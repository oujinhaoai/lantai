package httpapi

import (
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/schemas"
)

// Validate real response bytes against the source OpenAPI components. References
// load only embedded schemas; no external network or generated-file timing.
func apiCompiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	err := fs.WalkDir(schemas.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".schema.json") {
			return nil
		}
		raw, err := schemas.FS.ReadFile(path)
		if err != nil {
			return err
		}
		var v any
		if err = json.Unmarshal(raw, &v); err != nil {
			return err
		}
		return c.AddResource(schemas.BaseURI+path, v)
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := yamljson.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	var refs func(any)
	refs = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, y := range x {
				if k == "$ref" {
					if s, ok := y.(string); ok && strings.HasPrefix(s, "../schemas/") {
						x[k] = schemas.BaseURI + strings.TrimPrefix(s, "../schemas/")
					}
				} else {
					refs(y)
				}
			}
		case []any:
			for _, y := range x {
				refs(y)
			}
		}
	}
	refs(doc)
	if err = c.AddResource("https://api.example.test/openapi", doc); err != nil {
		t.Fatal(err)
	}
	return c
}
func validateResponse(t *testing.T, c *jsonschema.Compiler, name string, body []byte) {
	t.Helper()
	s, err := c.Compile("https://api.example.test/openapi#/components/schemas/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err = json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if err = s.Validate(v); err != nil {
		t.Fatalf("%s response violates source contract: %v\n%s", name, err, body)
	}
}

func TestActualResponsesMatchSourceSchemasAndUTCMilliseconds(t *testing.T) {
	f := newFixture(t)
	c := apiCompiler(t)
	w := call(f.h.API(), "GET", "/api/v1/meta", "", nil)
	validateResponse(t, c, "Meta", w.Body.Bytes())
	stamp := time.Date(2026, 1, 2, 11, 4, 5, 0, time.FixedZone("test", 8*3600))
	u := ids.New()
	f.storage.upload = storage.Upload{UploadID: u, OperationID: ids.New(), ProjectID: ids.New(), PrincipalID: f.identity.who.PrincipalID, State: storage.UploadOpen, CreatedAt: stamp, IdleExpiresAt: stamp.Add(time.Hour), ExpiresAt: stamp.Add(2 * time.Hour), PartsURL: storage.PartsURL(u), Files: []storage.UploadFile{{SHA256: strings.Repeat("a", 64), Size: 1, PartSize: 1024, PartCount: 1, State: storage.FilePending}}}
	w = call(f.h.API(), "GET", "/api/v1/uploads/"+string(u), "", bearer())
	validateResponse(t, c, "UploadView", w.Body.Bytes())
	if !strings.Contains(w.Body.String(), `"created_at":"2026-01-02T03:04:05.000Z"`) || !strings.Contains(w.Body.String(), `"received_parts":[]`) {
		t.Fatal(w.Body.String())
	}
	res := SessionResponse{Token: "lts_synthetic", Session: identity.SessionInfo{SessionID: ids.New(), PrincipalID: ids.New(), PrincipalKind: authz.Agent, Kind: "ordinary", Channel: "cli", Scopes: []identity.Scope{identity.ScopeRead}, CreatedAt: stamp, ExpiresAt: stamp.Add(time.Hour)}}
	data, err := marshalHTTP(res)
	if err != nil {
		t.Fatal(err)
	}
	validateResponse(t, c, "SessionResponse", data)
	w = call(f.h.API(), "GET", "/api/v1/operations/"+string(ids.New()), "", bearer())
	// The shared error schema is already imported as a source response ref.
	s, err := c.Compile(schemas.BaseURI + "common/v1/error.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var problem any
	_ = json.Unmarshal(w.Body.Bytes(), &problem)
	if err = s.Validate(problem); err != nil {
		t.Fatal(err)
	}
}

type customEncoding struct{ At time.Time }

func (customEncoding) MarshalJSON() ([]byte, error) { return []byte(`{"custom":"keep-me"}`), nil }

type EmbeddedTimes struct {
	At    time.Time
	Other time.Time
}

func TestHTTPTimeFormattingDoesNotRewriteUserStringsOrCustomJSON(t *testing.T) {
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	value := struct {
		EmbeddedTimes
		At       string
		Metadata map[string]any `json:"metadata"`
		Custom   customEncoding `json:"custom"`
	}{EmbeddedTimes: EmbeddedTimes{At: stamp, Other: stamp}, At: "literal", Metadata: map[string]any{"timestamp": "2026-01-02T03:04:05Z", "nested": []any{map[string]any{"actual_time": stamp}}}}
	raw, err := marshalHTTP(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["At"] != "literal" || out["Other"] != clock.Format(stamp) || out["metadata"].(map[string]any)["timestamp"] != "2026-01-02T03:04:05Z" || out["custom"].(map[string]any)["custom"] != "keep-me" {
		t.Fatal(string(raw))
	}
}
