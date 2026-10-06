package integration

import (
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

func publicationHistory(t *testing.T, f *flowEnv, asset ids.ID) int {
	t.Helper()
	var n int
	if err := f.inst.DB(ownership.Ledger).QueryRowContext(t.Context(), `SELECT count(*) FROM ledger_publications WHERE asset_id=?`, asset).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// After a real Flow has published and completed, replaying the succeeded
// publication request returns the first receipt without new history
// (BUG-20261001-06). A replay racing a withdrawal of the approval may only
// return that historical receipt: the suspended pointer is never revived.
func TestM2CompletedFlowPublicationReplayAndRevocationRace(t *testing.T) {
	f := newFlowEnv(t)
	ctx := t.Context()
	rules := f.approvedContext("released", []byte("# Released\n\nSynthetic release notes.\n"))
	control, err := f.ledger.AssetControl(ctx, rules.AssetID)
	if err != nil || control.PublicationState != "published" || control.PublishedVersionID != rules.VersionID {
		t.Fatal(control, err)
	}
	requests, err := f.reviews.PublicationRequests(ctx, f.owner, rules.AssetID)
	if err != nil || len(requests) != 1 || requests[0].Status != "succeeded" {
		t.Fatal(requests, err)
	}
	before := publicationHistory(t, f, rules.AssetID)
	// The dispatcher that ran the request replays it after the Flow completed.
	first, err := f.reviews.RunPublication(ctx, f.owner, requests[0].ID)
	if err != nil || first.ToVersionID != rules.VersionID || first.Action != "publish" {
		t.Fatal("replay after flow completion", first, err)
	}
	again, err := f.reviews.RunPublication(ctx, f.owner, requests[0].ID)
	if err != nil || again != first {
		t.Fatal("replays return the same receipt", again, first, err)
	}
	if n := publicationHistory(t, f, rules.AssetID); n != before {
		t.Fatal("replay added publication history", n, before)
	}

	// Withdraw the approval while replays run concurrently.
	who := f.login().Context
	state, err := f.ledger.VersionControl(ctx, rules.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	decision := ledger.ReviewDecision{Action: identity.ActRevokeReview, TargetID: state.ReviewTargetID, ExpectedRevision: state.Revision, Verdict: "revoke", EffectiveReviewID: state.EffectiveReviewID, Reason: "synthetic withdrawal", Waivers: []ledger.ReviewWaiver{}}
	action, err := f.reviews.HumanAction(ctx, decision)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := f.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{action}, f.reviews)
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.id.VerifyChallenge(ctx, who, ch.ChallengeID, f.fresh(), "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	items, err := f.id.DomainItems(ctx, who, g.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	replays := make([]error, 8)
	for i := range replays {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			p, err := f.reviews.RunPublication(ctx, f.owner, requests[0].ID)
			if err == nil && p != first {
				err = errcode.New(errcode.Internal, "replay returned a different receipt")
			}
			replays[i] = err
		}(i)
	}
	var revokeErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, revokeErr = f.reviews.Record(ctx, who, decision, g.GrantID, items[0].OperationID, f.id)
	}()
	close(start)
	wg.Wait()
	if revokeErr != nil {
		t.Fatal("withdrawal", revokeErr)
	}
	for i, err := range replays {
		if err != nil {
			t.Error("concurrent replay", i, err)
		}
	}
	control, err = f.ledger.AssetControl(ctx, rules.AssetID)
	if err != nil || control.PublicationState != "suspended" || control.PublishedVersionID != "" {
		t.Fatal("withdrawal must leave the pointer suspended", control, err)
	}
	if n := publicationHistory(t, f, rules.AssetID); n != before+1 {
		t.Fatal("history must add only the suspension", n, before)
	}
	// Replays after the withdrawal still return only the historical receipt.
	late, err := f.reviews.RunPublication(ctx, f.owner, requests[0].ID)
	if err != nil || late != first {
		t.Fatal("replay after withdrawal", late, err)
	}
	if control, err = f.ledger.AssetControl(ctx, rules.AssetID); err != nil || control.PublicationState != "suspended" || control.PublishedVersionID != "" {
		t.Fatal("replay revived the pointer", control, err)
	}
	if requests, err = f.reviews.PublicationRequests(ctx, f.owner, rules.AssetID); err != nil || requests[0].Status != "succeeded" {
		t.Fatal("request status", requests, err)
	}
}
