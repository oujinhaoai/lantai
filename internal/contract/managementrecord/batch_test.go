package managementrecord

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestBatchSharedFixtureAndSemanticGuards(t *testing.T) {
	raw, err := os.ReadFile("../../../schemas/examples/catalog/v1/migration-batch/valid.json")
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	base, err := DecodeBatch(example.Document)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*MigrationBatch){
		"missing rows": func(b *MigrationBatch) { b.ImportSummary.NotAttempted = 0 },
		"unsafe revision": func(b *MigrationBatch) {
			b.Revision = 9007199254740992
			b.PreviousRef = &Ref{AssetID: ids.New(), VersionID: ids.New()}
		},
		"UTF16 title":  func(b *MigrationBatch) { b.Title = strings.Repeat("😀", 101) },
		"client ready": func(b *MigrationBatch) { b.Acceptance = "ready" },
		"credential":   func(b *MigrationBatch) { b.Checklist["token"] = true },
		"duplicate keys": func(b *MigrationBatch) {
			a := BatchAsset{Key: "one", AssetID: ids.New(), VersionID: ids.New(), ManifestDigest: "sha256:" + strings.Repeat("a", 64), Status: "unknown"}
			b.Evidence.Assets = []BatchAsset{a, a}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var b MigrationBatch
			_ = json.Unmarshal(example.Document, &b)
			change(&b)
			raw, _ := json.Marshal(b)
			if _, err := DecodeBatch(raw); err == nil {
				t.Fatal("invalid batch accepted", base)
			}
		})
	}
}
