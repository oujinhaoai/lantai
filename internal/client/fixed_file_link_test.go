package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
)

type fixedFileTestTransport func(*http.Request) (*http.Response, error)

func (f fixedFileTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFixedFileLink(t *testing.T) {
	const asset = "01M80QVZBZM6Z03R52YT0RCPZC"
	const version = "01M80QVZBZKGD0G52Z5GVZAMD9"
	const instance = "01M80QVZBZKST1FKYX4BDN82JC"
	const file = "镜头/角色 +%&#.png"
	const endpoint = "/api/v1/assets/" + asset + "/versions/" + version
	for _, tc := range []struct {
		name, path              string
		denied, duplicate, want bool
	}{
		{"exact", file, false, false, true},
		{"other location", "另一个/角色 +%&#.png", false, false, false},
		{"permission denied", file, true, false, false},
		{"ambiguous duplicate", file, false, true, false},
		{"invalid", "../" + file, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Config{BaseURL: "https://files.example:8443", SessionToken: "synthetic-token"})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			c.api.Transport = fixedFileTestTransport(func(r *http.Request) (*http.Response, error) {
				count++
				if r.Method != "GET" || r.URL.Host != "files.example:8443" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Fatalf("unexpected request %v", r)
				}
				status := 200
				var body string
				switch r.URL.Path {
				case "/api/v1/meta":
					body = fmt.Sprintf(`{"api_version":"v1","instance_id":%q}`, instance)
				case endpoint:
					if r.URL.RawQuery != "view=full" {
						t.Fatalf("unexpected query: %v", r.URL)
					}
					if tc.denied {
						status = 403
						body = `{"code":"FORBIDDEN","message":"denied"}`
						break
					}
					files := []InputFile{{Path: file, SHA256: strings.Repeat("a", 64), Size: 3}}
					if tc.duplicate {
						files = append(files, files[0])
					}
					content, _ := json.Marshal(map[string]any{"files": files})
					content, _ = canonjson.Canonicalize(content)
					id := digest.Of(content)
					body = fmt.Sprintf(`{"version":{"asset_id":%q,"version_id":%q,"manifest_digest":%q},"manifest":{"asset_id":%q,"version_id":%q,"instance_id":%q,"manifest_digest":%q,"content":%s}}`, asset, version, id, asset, version, instance, id, content)
				default:
					t.Fatalf("unexpected request %v", r.URL)
				}
				return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			got, err := c.FixedFileLink(context.Background(), FixedFileLinkInput{AssetID: asset, VersionID: version, Path: tc.path})
			if tc.want {
				if err != nil {
					t.Fatal(err)
				}
				u, err := url.Parse(got.URL)
				if err != nil {
					t.Fatal(err)
				}
				fragment := u.EscapedFragment()
				prefix := "/assets/" + asset + "?"
				if !strings.HasPrefix(fragment, prefix) || u.RawQuery != "" || u.User != nil {
					t.Fatalf("unsafe or wrong href: %s", got.URL)
				}
				val, err := url.ParseQuery(strings.TrimPrefix(fragment, prefix))
				if err != nil || val.Get("file") != file || val.Get("version") != version {
					t.Fatalf("fragment did not roundtrip: %s %v", got.URL, err)
				}
				if count != 2 {
					t.Fatalf("requests: %d", count)
				}
			} else if err == nil || got.URL != "" || (tc.name == "invalid" && count != 0) {
				t.Fatalf("unexpected result: %+v, %v, %d reads", got, err, count)
			}
		})
	}
}
