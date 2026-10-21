package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// AssetFilter combines the existing server scope with exact, NFC-normalized,
// case-sensitive tag intersection. All Characters and Tags must be present.
// An empty dimension imposes no condition. These tags are descriptive, not an
// authorization policy or proof of approval.
type AssetFilter struct {
	ProjectID     string   `json:"project_id,omitempty"`
	AssetType     string   `json:"asset_type,omitempty"`
	Text          string   `json:"text,omitempty"`
	Characters    []string `json:"characters,omitempty"`
	Source        string   `json:"source,omitempty"`
	BusinessState string   `json:"business_state,omitempty"`
	LegacyVersion string   `json:"legacy_version,omitempty"`
	Tags          []string `json:"tags,omitempty"`
}

// AssetFindOptions bounds requests, returned authorized candidates and memory.
// Zero selects defaults (100, 1000, 100000, 10000, 2 respectively).
// MaxRestarts includes cursor expiration recovery, never authentication retry.
type AssetFindOptions struct {
	PageSize    int `json:"page_size,omitempty"`
	MaxPages    int `json:"max_pages,omitempty"`
	MaxItems    int `json:"max_items,omitempty"`
	MaxResults  int `json:"max_results,omitempty"`
	MaxRestarts int `json:"max_restarts,omitempty"`
}

// AssetFindItem is the existing public QueryItem projection. It carries no
// hidden candidate count, source path, arbitrary extra metadata or file grant.
type AssetFindItem struct {
	AssetID       string   `json:"asset_id"`
	ProjectID     string   `json:"project_id"`
	VersionID     string   `json:"version_id"`
	VersionNumber int64    `json:"version_number"`
	AssetType     string   `json:"asset_type"`
	Slug          string   `json:"slug"`
	Title         string   `json:"title"`
	Summary       string   `json:"summary,omitempty"`
	Tags          []string `json:"tags"`
	Subjects      []string `json:"subjects"`
	Revision      int64    `json:"revision"`
}

type AssetFindResult struct {
	Items     []AssetFindItem `json:"items"`
	Complete  bool            `json:"complete"`
	Truncated bool            `json:"truncated"`
	Pages     int             `json:"pages"`
	Scanned   int             `json:"scanned"`
	Restarts  int             `json:"restarts"`
}

var ErrAssetFindIncomplete = errors.New("client: asset search incomplete")

// AssetFindIncompleteError accompanies only a budget-limited partial result.
// A caller must check err/Complete before using results as a complete batch.
// Re-run with a larger explicit budget; no continuation token is persisted.
type AssetFindIncompleteError struct{ Reason string }

func (e *AssetFindIncompleteError) Error() string {
	return ErrAssetFindIncomplete.Error() + ": " + e.Reason
}
func (e *AssetFindIncompleteError) Unwrap() error { return ErrAssetFindIncomplete }

func assetTypeValid(t string) bool {
	return slices.Contains([]string{"image", "video", "audio", "doc", "config", "plugin", "model", "motion", "scene", "production"}, t)
}

func normalizeAssetFilter(f AssetFilter) (AssetFilter, []string, error) {
	if f.ProjectID != "" && !ids.ID(f.ProjectID).Valid() {
		return f, nil, errors.New("client: invalid project filter")
	}
	if f.AssetType != "" && !assetTypeValid(f.AssetType) {
		return f, nil, errors.New("client: invalid asset type filter")
	}
	var err error
	if f.Text, err = assetText(f.Text, 4096); err != nil || len(f.Text) > 4096 {
		return f, nil, errors.New("client: invalid text filter")
	}
	p, err := (AssetProfile{Characters: f.Characters, Source: f.Source, BusinessState: f.BusinessState, LegacyVersion: f.LegacyVersion}).Normalize()
	if err != nil {
		return f, nil, err
	}
	tags := p.tags()
	// A missing state is an unfiltered dimension, not a request for unknown.
	if f.BusinessState == "" {
		tags = slices.DeleteFunc(tags, func(s string) bool { return strings.HasPrefix(s, "state:") })
	}
	f.Characters, f.Source, f.LegacyVersion = p.Characters, p.Source, p.LegacyVersion
	f.Tags, err = assetStringSet(f.Tags, 64, 64)
	if err != nil {
		return f, nil, errors.New("client: invalid exact tags")
	}
	tags, err = assetStringSet(append(tags, f.Tags...), 64, 64)
	return f, tags, err
}

func normalizeAssetFindOptions(o AssetFindOptions) (AssetFindOptions, error) {
	options := []struct {
		value *int
		def   int
		max   int
	}{
		{&o.PageSize, 100, 100}, {&o.MaxPages, 1000, 10000},
		{&o.MaxItems, 100000, 1000000}, {&o.MaxResults, 10000, 100000},
		{&o.MaxRestarts, 2, 8},
	}
	for _, option := range options {
		if *option.value == 0 {
			*option.value = option.def
		}
		if *option.value < 1 || *option.value > option.max {
			return o, errors.New("client: asset search budget is outside allowed bounds")
		}
	}
	return o, nil
}

type assetSearchState struct {
	Generation   *int64 `json:"generation"`
	HighWater    *int64 `json:"high_water"`
	RebuildStart *int64 `json:"rebuild_start"`
}

type assetSearchPage struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
	State      *assetSearchState `json:"state"`
}

func decodeAssetSearchPage(r Response, limit int) (assetSearchPage, []AssetFindItem, error) {
	var p assetSearchPage
	if r.Status != http.StatusOK || decodeAssetJSON(r.Body, &p) != nil || p.Items == nil || len(p.Items) > limit || len(p.NextCursor) > 16384 || p.State == nil ||
		p.State.Generation == nil || *p.State.Generation < 1 || p.State.HighWater == nil || *p.State.HighWater < 0 || p.State.RebuildStart == nil || *p.State.RebuildStart < 0 {
		return p, nil, errors.New("client: invalid asset search page")
	}
	items := make([]AssetFindItem, 0, len(p.Items))
	for _, raw := range p.Items {
		var item AssetFindItem
		var fields map[string]json.RawMessage
		if decodeAssetJSON(raw, &item) != nil || json.Unmarshal(raw, &fields) != nil || !ids.ID(item.AssetID).Valid() || !ids.ID(item.ProjectID).Valid() || !ids.ID(item.VersionID).Valid() ||
			item.VersionNumber < 1 || !assetTypeValid(item.AssetType) || item.Slug == "" || item.Title == "" || item.Tags == nil || item.Subjects == nil || fields["revision"] == nil || bytes.Equal(bytes.TrimSpace(fields["revision"]), []byte("null")) || item.Revision < 0 {
			return p, nil, errors.New("client: invalid asset search item")
		}
		var err error
		if item.Tags, err = assetStringSet(item.Tags, 64, 64); err != nil {
			return p, nil, errors.New("client: invalid asset search tags")
		}
		if item.Subjects, err = assetStringSet(item.Subjects, 64, 64); err != nil {
			return p, nil, errors.New("client: invalid asset search subjects")
		}
		items = append(items, item)
	}
	return p, items, nil
}

// FindAssets traverses every current-authorized server page, including empty
// pages with a cursor, and intersects tags locally. It never counts hidden
// candidates. CURSOR_EXPIRED discards the old accumulation before restarting.
// Any other failure discards all accumulated items. Returned bytes cannot be
// recalled: callers must replace their display/cache and fresh reads still
// require server authorization. This is a paginated live view, not a snapshot.
func (c *Client) FindAssets(ctx context.Context, filter AssetFilter, options AssetFindOptions) (AssetFindResult, error) {
	f, tags, err := normalizeAssetFilter(filter)
	if err != nil {
		return AssetFindResult{}, err
	}
	o, err := normalizeAssetFindOptions(options)
	if err != nil {
		return AssetFindResult{}, err
	}
	result := AssetFindResult{Items: []AssetFindItem{}}
	query := url.Values{"view": {"brief"}, "limit": {strconv.Itoa(o.PageSize)}}
	if f.ProjectID != "" {
		query.Set("project_id", f.ProjectID)
	}
	if f.AssetType != "" {
		query.Set("asset_type", f.AssetType)
	}
	if f.Text != "" {
		query.Set("q", f.Text)
	}
	var cursor, lastID string
	var generation int64
	cursors := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return AssetFindResult{}, err
		}
		if result.Pages >= o.MaxPages {
			return incompleteAssetFind(result, "max_pages")
		}
		if result.Scanned >= o.MaxItems {
			return incompleteAssetFind(result, "max_items")
		}
		if cursor == "" {
			query.Del("cursor")
		} else {
			query.Set("cursor", cursor)
		}
		// Avoid receiving more authorized items than the explicit scan budget.
		limit := min(o.PageSize, o.MaxItems-result.Scanned)
		query.Set("limit", strconv.Itoa(limit))
		result.Pages++
		r, err := c.Do(ctx, http.MethodGet, "/api/v1/assets?"+query.Encode(), nil, Options{})
		if err != nil {
			var apiErr *APIError
			if cursor != "" && errors.As(err, &apiErr) && !apiErr.Protocol && apiErr.Body.Code == errcode.CursorExpired && result.Restarts < o.MaxRestarts {
				result.Restarts++
				result.Items = []AssetFindItem{}
				cursor, lastID, generation = "", "", 0
				cursors = map[string]bool{}
				continue
			}
			return AssetFindResult{}, err
		}
		page, items, err := decodeAssetSearchPage(r, limit)
		if err != nil {
			return AssetFindResult{}, err
		}
		if err := ctx.Err(); err != nil {
			return AssetFindResult{}, err
		}
		if generation != 0 && generation != *page.State.Generation {
			return AssetFindResult{}, errors.New("client: search generation changed without cursor expiration")
		}
		generation = *page.State.Generation
		for _, item := range items {
			if item.AssetID <= lastID || (f.ProjectID != "" && item.ProjectID != f.ProjectID) || (f.AssetType != "" && item.AssetType != f.AssetType) {
				return AssetFindResult{}, errors.New("client: search ordering or scope mismatch")
			}
			lastID = item.AssetID
			result.Scanned++
			if !slices.ContainsFunc(tags, func(tag string) bool { return !slices.Contains(item.Tags, tag) }) {
				if len(result.Items) == o.MaxResults {
					return incompleteAssetFind(result, "max_results")
				}
				result.Items = append(result.Items, item)
			}
		}
		if page.NextCursor == "" {
			result.Complete = true
			return result, nil
		}
		if cursors[page.NextCursor] {
			return AssetFindResult{}, errors.New("client: repeated search cursor")
		}
		cursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

func incompleteAssetFind(result AssetFindResult, reason string) (AssetFindResult, error) {
	result.Truncated = true
	return result, &AssetFindIncompleteError{Reason: reason}
}

// String makes budget diagnostics useful without including filters or paths.
func (r AssetFindResult) String() string {
	return fmt.Sprintf("asset search: complete=%t truncated=%t pages=%d scanned=%d results=%d restarts=%d", r.Complete, r.Truncated, r.Pages, r.Scanned, len(r.Items), r.Restarts)
}
