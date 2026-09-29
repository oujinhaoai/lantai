package ledger

import (
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestD18FixedGraceQuotaAndProtectionPolicy(t *testing.T) {
	now := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	actor := ids.New()
	base := func() trashFacts {
		return trashFacts{actor: authz.Context{PrincipalID: actor, PrincipalKind: authz.Agent}, versions: []trashVersionFacts{{id: ids.New(), author: actor, committed: now.Add(-48 * time.Hour), lifecycle: "active"}}, latestCommitted: now.Add(-time.Hour), firstCommitted: now.Add(-48 * time.Hour), retentionDays: 30, now: now}
	}
	f := base()
	d, err := decideTrash(f)
	if err != nil || !d.grace || d.retentionDays != 7 || d.quotaUnits != 1 {
		t.Fatal("fresh latest must cover an old owned intermediate", d, err)
	}
	f.latestCommitted = now.Add(-3 * time.Hour)
	d, err = decideTrash(f)
	if err != nil || d.grace || d.retentionDays != 30 {
		t.Fatal("3h boundary is not grace", d, err)
	}
	f.normalReserved = 20
	_, err = decideTrash(f)
	wantCode(t, err, errcode.QuotaExceeded)
	f = base()
	f.graceReserved = 199
	if _, err = decideTrash(f); err != nil {
		t.Fatal(err)
	}
	f.graceReserved = 200
	_, err = decideTrash(f)
	wantCode(t, err, errcode.QuotaExceeded)
	cases := []struct {
		name   string
		change func(*trashFacts)
		code   errcode.Code
	}{
		{"foreign", func(f *trashFacts) { f.versions[0].author = ids.New() }, errcode.Forbidden},
		{"approved", func(f *trashFacts) { f.versions[0].everApproved = true }, errcode.Forbidden},
		{"locked", func(f *trashFacts) { f.versions[0].locked = true }, errcode.AssetLocked},
		{"published", func(f *trashFacts) { f.versions[0].published = true }, errcode.AssetInUse},
		{"in-use", func(f *trashFacts) { f.inUse = true }, errcode.AssetInUse},
		{"directory", func(f *trashFacts) { f.directory = true }, errcode.HumanProofRequired},
		{"old-whole-asset", func(f *trashFacts) { f.wholeAsset = true }, errcode.HumanProofRequired},
		{"forged-force", func(f *trashFacts) { f.inUse = true; f.adminForceAuthorized = true }, errcode.HumanProofRequired},
		{"trash", func(f *trashFacts) { f.versions[0].lifecycle = "trashed" }, errcode.InvalidStateTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { f := base(); tc.change(&f); _, err := decideTrash(f); wantCode(t, err, tc.code) })
	}
	f = base()
	f.wholeAsset = true
	f.firstCommitted = now.Add(-2 * time.Hour)
	f.versions[0].committed = f.firstCommitted
	d, err = decideTrash(f)
	if err != nil || !d.releaseName || !d.grace {
		t.Fatal(d, err)
	}
	f = base()
	f.actor.PrincipalKind = authz.Human
	f.humanAuthorized = true
	f.adminForceAuthorized = true
	f.inUse = true
	f.wholeAsset = true
	f.versions[0].everApproved = true
	d, err = decideTrash(f)
	if err != nil || d.grace || d.releaseName || d.retentionDays != 30 {
		t.Fatal(d, err)
	}
	// Even confirmed admin force cannot bypass lock or current publication.
	f.versions[0].locked = true
	_, err = decideTrash(f)
	wantCode(t, err, errcode.AssetLocked)
}

func TestD18NormalBatchConfirmationThreshold(t *testing.T) {
	now := time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC)
	actor := ids.New()
	f := trashFacts{actor: authz.Context{PrincipalID: actor, PrincipalKind: authz.Human}, latestCommitted: now.Add(-4 * time.Hour), firstCommitted: now.Add(-4 * time.Hour), retentionDays: 30, now: now}
	for range 51 {
		f.versions = append(f.versions, trashVersionFacts{id: ids.New(), author: actor, committed: f.firstCommitted, lifecycle: "active"})
	}
	if _, err := decideTrash(f); errcode.CodeOf(err) != errcode.HumanProofRequired {
		t.Fatal("unconfirmed normal batch accepted", err)
	}
	f.humanAuthorized = true
	d, err := decideTrash(f)
	if err != nil || d.grace || d.retentionDays != 30 {
		t.Fatal(d, err)
	}
}
