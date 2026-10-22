package application

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/operations"
)

// ManagementInfo exposes only the operations-owned administrator projection.
// Authentication and the live human/admin read guard remain in HTTP.
func (a *App) ManagementInfo(ctx context.Context) (operations.ManagementInfo, error) {
	return a.Instance.ManagementInfo(ctx)
}
