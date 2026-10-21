package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestVersionHistoryReadsOldVersionsWhenLatestIsHidden(t *testing.T) {
	f := newFixture(t)
	a := f.create(f.agent, "history", manifest.TypeDoc, map[string][]byte{"README.txt": []byte("v1")})
	b := f.appendVersion(f.agent, a.AssetID, a.VersionID, manifest.TypeDoc, map[string][]byte{"README.txt": []byte("v2")})
	c := f.appendVersion(f.agent, a.AssetID, b.VersionID, manifest.TypeDoc, map[string][]byte{"README.txt": []byte("v3")})
	f.rights.Restrict(b.VersionID, authz.PurposeArchiveReview)
	f.rights.Restrict(c.VersionID, authz.PurposeArchiveReview)
	if _, err := f.svc.GetAsset(t.Context(), f.agent, a.AssetID); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("latest should be hidden: %v", err)
	}
	p, err := f.svc.ListVersions(t.Context(), f.agent, a.AssetID, "", 1)
	if err != nil || len(p.Items) != 1 || p.Items[0].VersionID != a.VersionID || p.NextCursor == "" {
		t.Fatalf("history=%+v %v", p, err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), string(b.VersionID)) || strings.Contains(string(raw), string(c.VersionID)) {
		t.Fatal("unreadable version identifiers leaked")
	}
	next, err := f.svc.ListVersions(t.Context(), f.agent, a.AssetID, p.NextCursor, 1)
	if err != nil || len(next.Items) != 0 || next.NextCursor != "" {
		t.Fatalf("hidden continuation=%+v %v", next, err)
	}
	f.rights.Clear(b.VersionID)
	f.rights.Clear(c.VersionID)
	next, err = f.svc.ListVersions(t.Context(), f.agent, a.AssetID, p.NextCursor, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].VersionID != b.VersionID {
		t.Fatalf("current rights not reevaluated: %+v %v", next, err)
	}
	f.az.Revoke(f.agent.PrincipalID, f.project.ProjectID)
	_, err = f.svc.ListVersions(t.Context(), f.agent, a.AssetID, p.NextCursor, 1)
	wantCode(t, err, errcode.TokenRevoked)
}

func TestVersionHistoryCursorBindsAssetSessionAndProcess(t *testing.T) {
	f := newFixture(t)
	a := f.create(f.agent, "one", manifest.TypeDoc, map[string][]byte{"x": []byte("one")})
	f.appendVersion(f.agent, a.AssetID, a.VersionID, manifest.TypeDoc, map[string][]byte{"x": []byte("two")})
	other := f.create(f.agent, "other", manifest.TypeDoc, map[string][]byte{"x": []byte("other")})
	p, err := f.svc.ListVersions(t.Context(), f.agent, a.AssetID, "", 1)
	if err != nil || p.NextCursor == "" {
		t.Fatal(err)
	}
	_, err = f.svc.ListVersions(t.Context(), f.agent, other.AssetID, p.NextCursor, 1)
	wantCode(t, err, errcode.CursorExpired)
	_, err = f.svc.ListVersions(t.Context(), f.newAgent(f.project.ProjectID), a.AssetID, p.NextCursor, 1)
	wantCode(t, err, errcode.CursorExpired)
	_, err = f.svc.ListVersions(t.Context(), f.agent, a.AssetID, "not-a-cursor", 1)
	wantCode(t, err, errcode.CursorExpired)
	bytes, _ := base64.RawURLEncoding.DecodeString(p.NextCursor)
	bytes[len(bytes)/2] ^= 1
	_, err = f.svc.ListVersions(t.Context(), f.agent, a.AssetID, base64.RawURLEncoding.EncodeToString(bytes), 1)
	wantCode(t, err, errcode.CursorExpired)
	copyService := *f.svc
	copyService.versionCursor, err = newVersionCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	_, err = copyService.ListVersions(t.Context(), f.agent, a.AssetID, p.NextCursor, 1)
	wantCode(t, err, errcode.CursorExpired)
	for _, n := range []int{-1, 0, 101} {
		_, err = f.svc.ListVersions(t.Context(), f.agent, a.AssetID, "", n)
		wantCode(t, err, errcode.SchemaInvalid)
	}
	_, err = f.svc.ListVersions(t.Context(), f.agent, ids.New(), "", 1)
	wantCode(t, err, errcode.NotFound)
}

func TestVersionHistoryOpaqueCursorDoesNotLeakPositionWidth(t *testing.T) {
	c, err := newVersionCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	v := versionCursorData{Asset: ids.New(), Principal: ids.New(), Session: ids.New(), AuthEpoch: 1, RecoveryEpoch: 1, After: 9}
	a, err := c.seal(v)
	if err != nil {
		t.Fatal(err)
	}
	v.After = 1000000000
	b, err := c.seal(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatal("opaque cursor length disclosed hidden scan position width")
	}
}

type historyBudgetLedger struct {
	Ledger
	records []commit.Committed
	calls   int
}

func (l *historyBudgetLedger) AssetVersions(_ context.Context, _ ids.ID, after int64, limit int) ([]commit.Committed, error) {
	l.calls++
	out := []commit.Committed{}
	for _, v := range l.records {
		if v.VersionNumber > after {
			out = append(out, v)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func TestVersionHistoryBoundsHiddenCandidateScanAndContinues(t *testing.T) {
	f := newFixture(t)
	a := f.create(f.agent, "hidden-budget", manifest.TypeDoc, map[string][]byte{"x": []byte("synthetic")})
	base, err := f.ledger.Version(t.Context(), a.AssetID, a.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	l := &historyBudgetLedger{Ledger: f.ledger}
	for n := int64(1); n <= 1025; n++ {
		v := base
		v.VersionID = ids.New()
		v.VersionNumber = n
		l.records = append(l.records, v)
		if n <= 1024 {
			f.rights.Restrict(v.VersionID, authz.PurposeArchiveReview)
		}
	}
	f.svc.ledger = l
	p, err := f.svc.ListVersions(t.Context(), f.agent, a.AssetID, "", 100)
	if err != nil || len(p.Items) != 0 || p.NextCursor == "" || l.calls != 11 {
		t.Fatalf("bounded hidden page=%+v calls=%d %v", p, l.calls, err)
	}
	next, err := f.svc.ListVersions(t.Context(), f.agent, a.AssetID, p.NextCursor, 100)
	if err != nil || len(next.Items) != 1 || next.Items[0].VersionID != l.records[1024].VersionID || next.NextCursor != "" {
		t.Fatalf("continuation=%+v %v", next, err)
	}
}
