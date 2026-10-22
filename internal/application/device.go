package application

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	dc "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func (a *App) DeviceClaim(ctx context.Context, w authz.Context, key string, in dc.Claim) (dc.ClaimResponse, error) {
	return a.Nodes.ClaimDevice(ctx, w, key, in)
}
func (a *App) DeviceRead(ctx context.Context, w authz.Context, id ids.ID) (dc.Device, error) {
	ctx, h, err := a.Instance.Gate().Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return dc.Device{}, err
	}
	defer h.Release()
	return a.Nodes.OwnedDevice(ctx, w, id, false)
}
func (a *App) DeviceList(ctx context.Context, w authz.Context, project, after ids.ID, limit int) (dc.DevicePage, error) {
	return a.Nodes.DeviceList(ctx, w, project, after, limit)
}
func (a *App) DeviceApprove(ctx context.Context, w authz.Context, key string, id ids.ID, in dc.ApproveDevice) (dc.Device, error) {
	return a.Nodes.ApproveDevice(ctx, w, key, id, in)
}
func (a *App) DeviceRevoke(ctx context.Context, w authz.Context, key string, id ids.ID, in dc.Control) (dc.Device, error) {
	return a.Nodes.RevokeDevice(ctx, w, key, id, in)
}
func (a *App) DeviceJobCreate(ctx context.Context, w authz.Context, key string, id ids.ID, in dc.CreateJob) (dc.JobView, error) {
	return a.Jobs.CreateDeviceJob(ctx, w, key, id, in)
}
func (a *App) DeviceJobs(ctx context.Context, w authz.Context, id, after ids.ID, limit int) (dc.JobPage, error) {
	return a.Jobs.DeviceJobs(ctx, w, id, after, limit)
}

// DeviceJob is an authority port, not an archived browser receipt. Consumers
// still separately verify current remote bytes, rights and recovery acceptance.
func (a *App) DeviceJob(ctx context.Context, w authz.Context, id ids.ID) (dc.JobView, error) {
	return a.Jobs.DeviceJob(ctx, w, id)
}
func (a *App) DeviceJobInspect(ctx context.Context, w authz.Context, key string, id, job ids.ID, in dc.InspectRequest) (dc.JobView, error) {
	return a.Jobs.InspectDeviceJob(ctx, w, key, id, job, in)
}
func (a *App) DeviceJobApprove(ctx context.Context, w authz.Context, key string, id, job ids.ID, in dc.ApproveJob) (dc.JobView, error) {
	return a.Jobs.ApproveDeviceJob(ctx, w, key, id, job, in)
}
func (a *App) DevicePoll(ctx context.Context, w authz.Context, key string, id ids.ID) (dc.Poll, error) {
	return a.Jobs.PollDevice(ctx, w, key, id)
}
func (a *App) DeviceJobHeartbeat(ctx context.Context, w authz.Context, key string, id, job ids.ID, in dc.LeaseRequest) (dc.Poll, error) {
	return a.Jobs.HeartbeatDeviceJob(ctx, w, key, id, job, in)
}
func (a *App) DeviceJobCancel(ctx context.Context, w authz.Context, key string, id, job ids.ID, in dc.Control) (dc.JobView, error) {
	return a.Jobs.CancelDeviceJob(ctx, w, key, id, job, in)
}
func (a *App) DeviceJobResult(ctx context.Context, w authz.Context, key string, id, job ids.ID, in dc.ResultRequest) (dc.JobView, error) {
	return a.Jobs.ResultDeviceJob(ctx, w, key, id, job, in)
}
