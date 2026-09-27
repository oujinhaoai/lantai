package install

import (
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

func TestRequestPathLengthMatchesSchemaCharacters(t *testing.T) {
	reg, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path string
		valid      bool
	}{
		{"multibyte", strings.Repeat("测", 70), true},
		{"ascii boundary", strings.Repeat("a", 200), true},
		{"unicode boundary", strings.Repeat("测", 200), true},
		{"ascii excess", strings.Repeat("a", 201), false},
		{"unicode excess", strings.Repeat("测", 201), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := ids.New()
			req := Request{OperationID: id, ProjectID: id, AssetID: id, VersionID: id,
				VersionNumber: 1, ManifestDigest: digest.Of([]byte("manifest")),
				Files: []File{{Path: test.path, SHA256: strings.Repeat("a", 64), Size: 1}}}
			if err := reg.Validate("lantai.common-defs/v1#/$defs/relative_path", test.path); (err == nil) != test.valid {
				t.Fatalf("schema valid=%v: %v", test.valid, err)
			}
			if err := req.Validate(); (err == nil) != test.valid {
				t.Fatalf("request valid=%v: %v", test.valid, err)
			}
			req.Files[0].Path = "invalid-\xff"
			if err := req.Validate(); err == nil {
				t.Fatal("invalid UTF-8 path was accepted")
			}
		})
	}
}
