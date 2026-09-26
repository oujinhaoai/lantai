package committest

import "testing"

func TestMemoryLedgerSatisfiesContract(t *testing.T) {
	RunLedgerContract(t, NewHarness)
}
