package commit

import (
	"context"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// AliasAllocation is the immutable registration that authorizes a catalog alias
// history file. Asset is a snapshot at ALLOCATION time, not the current asset
// description: restoring under a new path never changes the old generation.
type AliasAllocation struct {
	Sequence int64  `json:"sequence"`
	Asset    Asset  `json:"asset"`
	Reason   string `json:"reason"`
}
type AliasHistory interface {
	AliasAllocation(context.Context, ids.ID, string, int64) (AliasAllocation, error)
	AliasAllocations(context.Context, int64, int) ([]AliasAllocation, error)
}
