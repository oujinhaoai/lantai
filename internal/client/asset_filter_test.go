package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func assetFindFixture(n int, tags ...string) AssetFindItem {
	return AssetFindItem{AssetID: fmt.Sprintf("%026d", n+1), ProjectID: "00000000000000000000000001", VersionID: fmt.Sprintf("%026d", n+5000), VersionNumber: 1, AssetType: "image", Slug: fmt.Sprintf("synthetic-%04d", n), Title: "Synthetic", Tags: append([]string{}, tags...), Subjects: []string{}, Revision: 0}
}

func assetFindPage(w http.ResponseWriter, items []AssetFindItem, next string, generation int) {
	writeJSON(w, map[string]any{"items": items, "next_cursor": next, "state": map[string]int{"generation": generation, "high_water": 10, "rebuild_start": 1}})
}

func assetFindAPIError(w http.ResponseWriter, status int, code errcode.Code) {
	w.WriteHeader(status)
	writeJSON(w, errcode.New(code, "synthetic rejection").Envelope(""))
}

func TestFindAssetsExactAND1205AndEmptyAuthorizedPage(t *testing.T) {
	all := make([]AssetFindItem, 1205)
	var want []string
	for i := range all {
		tags := []string{"character:hero-a", "state:active", "source:synthetic"}
		switch i % 4 {
		case 1:
			tags[0] = "character:hero-ab"
		case 2:
			tags[1] = "state:reference"
		case 3:
			tags[2] = "source:synthetic-other"
		default:
			want = append(want, fmt.Sprintf("%026d", i+1))
		}
		all[i] = assetFindFixture(i, tags...)
	}
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/v1/assets" || r.Header.Get("Authorization") != "Bearer private-session" || r.URL.Query().Get("project_id") != all[0].ProjectID || r.URL.Query().Get("asset_type") != "image" || r.URL.Query().Get("q") != "Synthetic" {
			t.Errorf("incorrect live request scope: %s", r.URL.Path)
		}
		cursor := r.URL.Query().Get("cursor")
		if cursor == "" {
			// Model a fully authorization-filtered page. Its hidden candidate
			// count is not exposed to the client.
			assetFindPage(w, []AssetFindItem{}, "start", 1)
			return
		}
		start := 0
		if cursor != "start" {
			start, _ = strconv.Atoi(cursor)
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit < 1 || limit > 100 {
			// Match the public HTTP boundary, which is narrower than the
			// internal query service's 200-item limit.
			assetFindAPIError(w, http.StatusBadRequest, errcode.SchemaInvalid)
			return
		}
		end := min(start+limit, len(all))
		next := ""
		if end < len(all) {
			next = strconv.Itoa(end)
		}
		assetFindPage(w, all[start:end], next, 1)
	})
	out, err := c.FindAssets(t.Context(), AssetFilter{ProjectID: all[0].ProjectID, AssetType: "image", Text: "Synthetic", Characters: []string{"hero-a"}, BusinessState: "active", Source: "synthetic"}, AssetFindOptions{})
	wantRequests := 1 + (len(all)+100-1)/100
	if err != nil || !out.Complete || out.Truncated || out.Scanned != 1205 || requests != wantRequests {
		t.Fatalf("incomplete traversal: %s %v requests=%d", out, err, requests)
	}
	got := make([]string, len(out.Items))
	for i, item := range out.Items {
		got[i] = item.AssetID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exact AND set mismatch: got %d want %d", len(got), len(want))
	}
}

func TestFindAssetsExpiredCursorDiscardsPreviouslyVisibleResults(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			assetFindPage(w, []AssetFindItem{assetFindFixture(0)}, "old", 1)
		case 2:
			assetFindAPIError(w, http.StatusConflict, errcode.CursorExpired)
		case 3:
			if r.URL.Query().Get("cursor") != "" {
				t.Error("expired cursor reused")
			}
			// Earlier item is now absent/revoked; it must not survive restart.
			assetFindPage(w, []AssetFindItem{assetFindFixture(4)}, "", 2)
		default:
			t.Error("unexpected request")
		}
	})
	out, err := c.FindAssets(t.Context(), AssetFilter{}, AssetFindOptions{})
	if err != nil || !out.Complete || len(out.Items) != 1 || out.Items[0].AssetID != assetFindFixture(4).AssetID || out.Restarts != 1 || requests != 3 {
		t.Fatalf("restart reused stale data: %+v %v", out, err)
	}
}

func TestFindAssetsAuthenticationRevocationAndProtocolErrorsDiscardItems(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   errcode.Code
		status int
	}{{"expired", errcode.TokenExpired, 401}, {"revoked", errcode.TokenRevoked, 401}, {"forbidden", errcode.Forbidden, 403}, {"cursor-exhausted", errcode.CursorExpired, 409}} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 || (tc.code == errcode.CursorExpired && requests == 3) {
					assetFindPage(w, []AssetFindItem{assetFindFixture(0)}, "cursor", requests)
					return
				}
				assetFindAPIError(w, tc.status, tc.code)
			})
			out, err := c.FindAssets(t.Context(), AssetFilter{}, AssetFindOptions{MaxRestarts: 1})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Body.Code != tc.code || len(out.Items) != 0 || out.Complete || out.Truncated {
				t.Fatalf("failure returned usable old items: %+v %v", out, err)
			}
			if tc.code != errcode.CursorExpired && requests != 2 {
				t.Fatal("authentication failure retried")
			}
		})
	}
}

func TestFindAssetsBudgetsAreExplicit(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts AssetFindOptions
		want string
	}{{"pages", AssetFindOptions{MaxPages: 1}, "max_pages"}, {"items", AssetFindOptions{MaxItems: 1}, "max_items"}, {"results", AssetFindOptions{MaxResults: 1}, "max_results"}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "results" {
					assetFindPage(w, []AssetFindItem{assetFindFixture(0), assetFindFixture(1)}, "", 1)
				} else {
					assetFindPage(w, []AssetFindItem{assetFindFixture(0)}, "next", 1)
				}
			})
			out, err := c.FindAssets(t.Context(), AssetFilter{}, tc.opts)
			var incomplete *AssetFindIncompleteError
			if !errors.Is(err, ErrAssetFindIncomplete) || !errors.As(err, &incomplete) || incomplete.Reason != tc.want || out.Complete || !out.Truncated || len(out.Items) != 1 {
				t.Fatalf("budget silently truncated: %+v %v", out, err)
			}
		})
	}
	// Exactly reaching a budget on the terminal page is complete.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assetFindPage(w, []AssetFindItem{assetFindFixture(0)}, "", 1)
	})
	out, err := c.FindAssets(t.Context(), AssetFilter{}, AssetFindOptions{MaxPages: 1, MaxItems: 1, MaxResults: 1})
	if err != nil || !out.Complete || out.Truncated {
		t.Fatalf("terminal page incorrectly truncated: %+v %v", out, err)
	}
}

func TestFindAssetsRejectsMalformedResponsesAndCancellation(t *testing.T) {
	item, _ := json.Marshal(assetFindFixture(0))
	for _, raw := range []string{
		`{}`, `null`, `[]`, `{"items":[],"state":{"generation":1,"high_water":0}}`,
		`{"items":null,"state":{"generation":1,"high_water":0,"rebuild_start":0}}`,
		`{"items":[],"state":{"generation":0,"high_water":0,"rebuild_start":0}}`,
		`{"items":[{}],"state":{"generation":1,"high_water":0,"rebuild_start":0}}`,
		`{"items":[` + strings.Replace(string(item), `"revision":0`, `"revision":null`, 1) + `],"state":{"generation":1,"high_water":0,"rebuild_start":0}}`,
		`{"items":[` + string(item) + `,` + string(item) + `],"state":{"generation":1,"high_water":0,"rebuild_start":0}}`,
		`{"items":[],"state":{"generation":1,"high_water":0,"rebuild_start":0},"hidden_count":1234}`,
	} {
		t.Run(fmt.Sprintf("malformed-%d", len(raw)), func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(raw)) })
			out, err := c.FindAssets(t.Context(), AssetFilter{}, AssetFindOptions{})
			if err == nil || len(out.Items) != 0 {
				t.Fatalf("accepted malformed response: %s", raw)
			}
		})
	}
	requests := 0
	ctx, cancel := context.WithCancel(t.Context())
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		cancel()
		assetFindPage(w, []AssetFindItem{assetFindFixture(0)}, "next", 1)
	})
	if out, err := c.FindAssets(ctx, AssetFilter{}, AssetFindOptions{}); !errors.Is(err, context.Canceled) || len(out.Items) != 0 || requests != 1 {
		t.Fatalf("cancellation retained results: %+v %v", out, err)
	}
}

func TestFindAssetsRepeatedCursorAndGenerationScope(t *testing.T) {
	for _, mode := range []string{"loop", "generation", "scope", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					assetFindPage(w, []AssetFindItem{}, "same", 1)
					return
				}
				switch mode {
				case "loop":
					assetFindPage(w, []AssetFindItem{}, "same", 1)
				case "generation":
					assetFindPage(w, []AssetFindItem{}, "", 2)
				case "scope":
					item := assetFindFixture(0)
					item.AssetType = "video"
					assetFindPage(w, []AssetFindItem{item}, "", 1)
				case "oversized":
					assetFindPage(w, []AssetFindItem{assetFindFixture(0), assetFindFixture(1)}, "", 1)
				}
			})
			if out, err := c.FindAssets(t.Context(), AssetFilter{AssetType: "image"}, AssetFindOptions{PageSize: 1}); err == nil || len(out.Items) != 0 || requests != 2 {
				t.Fatalf("invalid page accepted: %+v %v", out, err)
			}
		})
	}
}

func TestFindAssetsValidatesFiltersBeforeNetwork(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid filter reached network") })
	for _, filter := range []AssetFilter{{ProjectID: "bad"}, {AssetType: "new"}, {BusinessState: "approved"}, {Tags: []string{""}}} {
		if _, err := c.FindAssets(t.Context(), filter, AssetFindOptions{}); err == nil {
			t.Fatalf("accepted filter %+v", filter)
		}
	}
	for _, options := range []AssetFindOptions{{PageSize: 101}, {PageSize: 200}, {MaxItems: -1}, {MaxResults: 100001}, {MaxPages: 10001}, {MaxRestarts: 9}} {
		if _, err := c.FindAssets(t.Context(), AssetFilter{}, options); err == nil {
			t.Fatalf("accepted budget %+v", options)
		}
	}
}
