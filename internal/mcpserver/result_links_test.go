package mcpserver

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestResultLinksUsePublicVersionShape(t *testing.T) {
	ref := ids.PermanentRef{InstanceID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", AssetID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", VersionID: "01ARZ3NDEKTSV4RRFFQ69G5FAX"}
	uri, err := ref.URI()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		value any
		links int
	}{
		{"commit_receipt", map[string]any{"ref": ref, "uri": "https://untrusted.invalid/ignored"}, 1},
		{"exact_version_view", map[string]any{"version": map[string]any{"ref": ref}}, 1},
		{"unrelated_nested_ref", map[string]any{"metadata": map[string]any{"ref": ref}}, 0},
		{"invalid_version_ref", map[string]any{"version": map[string]any{"ref": map[string]string{"instance_id": "invalid"}}}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			out, err := toolResult(client.Response{Body: raw}, nil)
			if err != nil || out.IsError {
				t.Fatal("public result refused", err)
			}
			original := out.Content[0].(*mcp.TextContent).Text
			if original != string(raw) {
				t.Fatal("resource annotation changed the REST response")
			}
			encoded, _ := json.Marshal(out.StructuredContent)
			var originalValue any
			if err := json.Unmarshal(raw, &originalValue); err != nil {
				t.Fatal(err)
			}
			canonical, _ := json.Marshal(originalValue)
			if !bytes.Equal(encoded, canonical) {
				t.Fatal("resource annotation changed structured content")
			}
			links := 0
			for _, item := range out.Content {
				if link, ok := item.(*mcp.ResourceLink); ok {
					links++
					if link.URI != uri {
						t.Fatal("link trusted an arbitrary URI or lost exact IDs")
					}
				}
			}
			if links != test.links {
				t.Fatalf("links=%d, want=%d", links, test.links)
			}
		})
	}
}
