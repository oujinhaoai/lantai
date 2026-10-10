package httpapi

import (
	"net/http"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// T01 owns milestone configuration; T05 supplies authorized progress facts.
// The transport never stores task state or accesses either owner's tables.
func (h *Handler) milestoneRoutes(m *http.ServeMux) {
	if h.deps.Milestones == nil || h.deps.Tasks == nil {
		return
	}
	h.route(m, "GET /api/v1/projects/{project_id}/milestones", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		project, err := pathID(r, "project_id")
		if err != nil {
			return err
		}
		items, err := h.deps.Milestones.ListMilestones(r.Context(), who, project)
		if err != nil {
			return err
		}
		return respond(w, 200, map[string]any{"items": items})
	})
	h.route(m, "POST /api/v1/projects/{project_id}/milestones", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		project, err := pathID(r, "project_id")
		if err != nil {
			return err
		}
		key, err := key(r)
		if err != nil {
			return err
		}
		var in struct {
			ExpectedRevision *int64             `json:"expected_revision"`
			Milestone        identity.Milestone `json:"milestone"`
		}
		if err = h.decode(w, r, &in); err != nil {
			return err
		}
		if in.ExpectedRevision == nil || in.Milestone.TaskIDs == nil {
			return invalid("expected_revision and task_ids are required")
		}
		if in.Milestone.ProjectID != project {
			return invalid("milestone project must match the route")
		}
		result, err := h.deps.Milestones.PutMilestone(r.Context(), who, key, *in.ExpectedRevision, in.Milestone, h.deps.Tasks)
		if err != nil {
			return err
		}
		return respond(w, 200, result)
	})
	h.route(m, "GET /api/v1/projects/{project_id}/milestones/{id}/progress", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		project, err := pathID(r, "project_id")
		if err != nil {
			return err
		}
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		progress, err := h.deps.Milestones.MilestoneProgress(r.Context(), who, project, id, h.deps.Tasks)
		if err != nil {
			return err
		}
		return respond(w, 200, progress)
	})
}
