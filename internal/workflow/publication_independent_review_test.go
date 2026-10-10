package workflow

import (
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

// Delayed publication notifications cannot release a dependency for another
// version or production round, or reopen an already completed publish step.
// No database is supplied: all rejected notifications must return before any
// durable command or step change. The acceptance suite covers valid dispatch.
func TestPublicationIndependentReviewIgnoresInapplicableFacts(t *testing.T) {
	approved := ids.New()
	for _, test := range []struct {
		name    string
		version ids.ID
		state   string
		round   int
		step    tc.StepState
	}{
		{"other-version", ids.New(), "published", 2, "waiting"},
		{"withdrawn-publication", approved, "unpublished", 2, "waiting"},
		{"prior-production-round", approved, "published", 1, "waiting"},
		{"completed-step", approved, "published", 2, "completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := flowRow{meta: FlowMeta{ProductionRound: 2, ApprovedVersion: approved,
				Definition: Definition{Steps: []StepDef{{Key: "produce", Kind: "task"}, {Key: "review", Kind: "review"}, {Key: "publish", Kind: "publish"}}}},
				steps: []stepRow{{run: StepRun{StepKey: "publish", Round: 1, State: test.step}, meta: StepMeta{ProductionRound: test.round}}}}
			before := encode(r.flow) + encode(r.meta) + r.steps[0].snapshot()
			var w Service
			notes, err := w.onPublished(t.Context(), nil, &r, ledger.AssetControl{PublicationState: test.state, PublishedVersionID: test.version}, ids.New())
			if err != nil || len(notes) != 0 || before != encode(r.flow)+encode(r.meta)+r.steps[0].snapshot() {
				t.Fatal("inapplicable fact mutated workflow", notes, err, r)
			}
		})
	}
}
