package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type IssueDependencyResponse struct {
	SourceRevision        int64                             `json:"source_revision"`
	SourceEvidenceVersion string                            `json:"source_evidence_version"`
	ID                    string                            `json:"id"`
	Type                  string                            `json:"type"`
	Issue                 map[string]any                    `json:"issue"`
	Condition             issueworkflow.DependencyCondition `json:"condition"`
	TargetStep            string                            `json:"target_step"`
	Satisfied             bool                              `json:"satisfied"`
	Reason                string                            `json:"reason"`
	Provenance            json.RawMessage                   `json:"provenance"`
	CreatedAt             time.Time                         `json:"created_at"`
	RemovedAt             *time.Time                        `json:"removed_at"`
}

// Both endpoints must be visible in the selected workspace. A relation never
// grants access to its other endpoint, even for historical malformed rows.
func (h *Handler) loadDependencyIssue(w http.ResponseWriter, r *http.Request, id string) (db.Issue, bool) {
	if _, ok := h.workspaceMember(w, r, h.resolveWorkspaceID(r)); !ok {
		return db.Issue{}, false
	}
	return h.loadIssueForUser(w, r, id)
}

func dependencyEndpoints(d db.IssueDependency) (dependent, prerequisite pgtype.UUID) {
	if d.Type == "blocks" {
		return d.DependsOnIssueID, d.IssueID
	}
	return d.IssueID, d.DependsOnIssueID
}

func (h *Handler) ListIssueDependencies(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadDependencyIssue(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, err := h.Queries.ListIssueDependencies(r.Context(), db.ListIssueDependenciesParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, IncludeRemoved: r.URL.Query().Get("include_removed") == "true",
	})
	if err != nil {
		writeError(w, 500, "could not load relations")
		return
	}
	out := make([]IssueDependencyResponse, 0, len(rows))
	for _, row := range rows {
		otherID := row.DependsOnIssueID
		if otherID == issue.ID {
			otherID = row.IssueID
		}
		other, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: otherID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			continue
		}
		dependent, _ := dependencyEndpoints(row)
		direction := row.Type
		if row.Type != "related" {
			direction = "blocks"
			if dependent == issue.ID {
				direction = "blocked_by"
			}
		}
		step := issue.Status
		if dependent == other.ID {
			step = other.Status
		}
		if row.TargetStep != "" {
			step = row.TargetStep
		}
		reason := ""
		if row.Type != "related" && !row.RemovedAt.Valid {
			waits, err := h.Queries.IssueDependencyWaits(r.Context(), db.IssueDependencyWaitsParams{IssueID: dependent, Step: step})
			if err != nil {
				writeError(w, 500, "could not evaluate dependencies")
				return
			}
			for _, wait := range waits {
				if wait.DependencyID == row.ID {
					reason = wait.Reason
				}
			}
		}
		var condition issueworkflow.DependencyCondition
		if err := json.Unmarshal(row.Condition, &condition); err != nil {
			reason = "unsupported_condition"
		}
		var removed *time.Time
		if row.RemovedAt.Valid {
			removed = &row.RemovedAt.Time
		}
		var sourceRevision int64
		sourceVersion := ""
		if condition.Kind == "accepted" {
			_, sourceID := dependencyEndpoints(row)
			source := issue
			if sourceID == other.ID {
				source = other
			}
			sourceRevision = int64(source.Revision)
			sourceVersion, err = h.Queries.IssueEvidenceVersion(r.Context(), sourceID)
			if err != nil {
				writeError(w, 500, "could not resolve source evidence")
				return
			}
		}
		out = append(out, IssueDependencyResponse{ID: uuidToString(row.ID), Type: direction,
			Issue:     map[string]any{"id": uuidToString(other.ID), "identifier": h.dependencyIssueIdentifier(r.Context(), other), "title": other.Title, "status": other.Status, "status_category": issuestatus.WireCategory(other.Status, issuestatus.Category(r.Context(), h.Queries, other.WorkspaceID, other.Status)), "revision": other.Revision},
			Condition: condition, SourceRevision: sourceRevision, SourceEvidenceVersion: sourceVersion, TargetStep: row.TargetStep, Satisfied: reason == "", Reason: reason,
			Provenance: row.Provenance, CreatedAt: row.CreatedAt.Time, RemovedAt: removed})
	}
	ready, err := h.Queries.IssueDependenciesReady(r.Context(), db.IssueDependenciesReadyParams{IssueID: issue.ID, Step: issue.Status})
	if err != nil {
		writeError(w, 500, "could not evaluate readiness")
		return
	}
	reason := ""
	if !ready {
		category := issuestatus.Category(r.Context(), h.Queries, issue.WorkspaceID, issue.Status)
		if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
			reason = "issue_closed"
		} else {
			waits, err := h.Queries.IssueDependencyWaits(r.Context(), db.IssueDependencyWaitsParams{IssueID: issue.ID, Step: issue.Status})
			if err != nil {
				writeError(w, 500, "could not evaluate readiness")
				return
			}
			for _, wait := range waits {
				if wait.Reason == "unavailable" {
					reason = "unavailable"
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"relations": out, "ready": ready, "step": issue.Status, "reason": reason})
}

type CreateIssueDependencyRequest struct {
	IssueID    string                            `json:"issue_id"`
	Type       string                            `json:"type"`
	Condition  issueworkflow.DependencyCondition `json:"condition"`
	TargetStep string                            `json:"target_step"`
}

func (h *Handler) CreateIssueDependency(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadDependencyIssue(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req CreateIssueDependencyRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, 400, "invalid relation")
		return
	}
	other, ok := h.loadIssueForUser(w, r, req.IssueID)
	if !ok {
		return
	}
	if other.ID == issue.ID {
		writeErrorCode(w, 400, "dependency_self_link", "an issue cannot link to itself")
		return
	}
	if req.Type == "" {
		req.Type = "blocked_by"
	}
	if req.Type != "blocked_by" && req.Type != "blocks" && req.Type != "related" {
		writeError(w, 400, "type must be blocked_by, blocks or related")
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(issue.WorkspaceID))
	if req.Type != "related" && (actorType != "member" || isMachineCredentialActor(r)) {
		writeError(w, 403, "execution dependency changes require human credentials")
		return
	}
	req.TargetStep = strings.TrimSpace(req.TargetStep)
	req.Condition.SourceStep = strings.TrimSpace(req.Condition.SourceStep)
	if req.Condition.Kind == "" {
		req.Condition.Kind = "done"
	}
	if err := req.Condition.Validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	dependent, prerequisite := issue, other
	if req.Type == "blocks" {
		dependent, prerequisite = other, issue
	}
	if req.Type == "related" && (req.TargetStep != "" || req.Condition.Kind != "done") {
		writeError(w, 400, "context links cannot carry execution conditions")
		return
	}
	if req.TargetStep != "" {
		def, _, err := issueworkflow.ForProject(r.Context(), h.Queries, dependent.WorkspaceID, dependent.ProjectID)
		if err != nil {
			writeError(w, 500, "could not load workflow")
			return
		}
		if def == nil || !def.Has(req.TargetStep) {
			writeError(w, 400, "target_step must name a step in the dependent issue's workflow")
			return
		}
	}
	if req.Condition.Kind == "run_completed" {
		run, err := h.Queries.GetIssueProvenanceRun(r.Context(), db.GetIssueProvenanceRunParams{RunID: parseUUID(req.Condition.RunID), IssueID: prerequisite.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			writeError(w, 404, "source run not found")
			return
		}
		req.Condition.RunID = uuidToString(run.ID)
		if req.Condition.SourceStep != "" && provenanceEntryStep(run.Context) != req.Condition.SourceStep {
			writeError(w, 400, "source run belongs to another step")
			return
		}
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "could not save relation")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	if err = q.LockIssueDependencyGraph(r.Context(), issue.WorkspaceID); err != nil {
		writeError(w, 500, "could not lock relations")
		return
	}
	existing, err := q.ListIssueDependencies(r.Context(), db.ListIssueDependenciesParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		writeError(w, 500, "could not load relations")
		return
	}
	condition, _ := json.Marshal(req.Condition)
	for _, row := range existing {
		d, p := dependencyEndpoints(row)
		same := d == dependent.ID && p == prerequisite.ID && row.Type != "related" && req.Type != "related"
		if req.Type == "related" && row.Type == "related" {
			same = (row.IssueID == issue.ID && row.DependsOnIssueID == other.ID) || (row.IssueID == other.ID && row.DependsOnIssueID == issue.ID)
		}
		if same && row.TargetStep == req.TargetStep {
			var old issueworkflow.DependencyCondition
			if json.Unmarshal(row.Condition, &old) == nil && old == req.Condition {
				writeJSON(w, 200, map[string]any{"id": uuidToString(row.ID), "created": false})
			} else {
				writeErrorCode(w, 409, "dependency_condition_conflict", "remove the existing condition before replacing it")
			}
			return
		}
	}
	if req.Type != "related" {
		if !h.dependencyEditable(w, r, q, dependent.ID) {
			return
		}
		edges, err := q.ListWorkspaceExecutionEdges(r.Context(), issue.WorkspaceID)
		if err != nil {
			writeError(w, 500, "could not check dependency graph")
			return
		}
		graph := make([]issueworkflow.DependencyEdge, 0, len(edges))
		for _, row := range edges {
			d, p := dependencyEndpoints(row)
			graph = append(graph, issueworkflow.DependencyEdge{Dependent: uuidToString(d), Prerequisite: uuidToString(p)})
		}
		cycle := issueworkflow.DependencyCycle(graph, issueworkflow.DependencyEdge{Dependent: uuidToString(dependent.ID), Prerequisite: uuidToString(prerequisite.ID)})
		if len(cycle) > 0 {
			for i, id := range cycle {
				if row, err := q.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: parseUUID(id), WorkspaceID: issue.WorkspaceID}); err == nil {
					cycle[i] = h.dependencyIssueIdentifier(r.Context(), row)
				}
			}
			writeJSON(w, 409, map[string]any{"code": "dependency_cycle", "error": "dependency cycle: " + strings.Join(cycle, " → "), "chain": cycle})
			return
		}
	}
	provenance, _ := json.Marshal(map[string]string{"source": "manual", "actor_type": actorType, "actor_id": actorID})
	typ := "blocked_by"
	if req.Type == "related" {
		typ = "related"
	}
	row, err := q.CreateIssueDependency(r.Context(), db.CreateIssueDependencyParams{ID: dbid.NewV7(), IssueID: dependent.ID, DependsOnIssueID: prerequisite.ID, Type: typ, Condition: condition, TargetStep: req.TargetStep, Provenance: provenance})
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "could not save relation")
		return
	}
	h.publishIssueRelationChanges(issue, other)
	writeJSON(w, 201, map[string]any{"id": uuidToString(row.ID), "created": true})
}

func (h *Handler) dependencyEditable(w http.ResponseWriter, r *http.Request, q *db.Queries, issueID pgtype.UUID) bool {
	active, err := q.HasActiveDependencyRun(r.Context(), issueID)
	if err != nil {
		writeError(w, 500, "could not check active runs")
		return false
	}
	if active {
		writeErrorCode(w, 409, "dependency_run_active", "finish or cancel the dependent issue's active run before changing execution dependencies")
		return false
	}
	return true
}

func (h *Handler) RemoveIssueDependency(w http.ResponseWriter, r *http.Request) {
	h.mutateIssueDependency(w, r, false)
}

func (h *Handler) AcceptIssueDependency(w http.ResponseWriter, r *http.Request) {
	h.mutateIssueDependency(w, r, true)
}

func (h *Handler) mutateIssueDependency(w http.ResponseWriter, r *http.Request, accept bool) {
	issue, ok := h.loadDependencyIssue(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "relationID"), "relation id")
	if !ok {
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(issue.WorkspaceID))
	if accept && (actorType != "member" || isMachineCredentialActor(r)) {
		writeError(w, 403, "human acceptance is required")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "could not update relation")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	if err = q.LockIssueDependencyGraph(r.Context(), issue.WorkspaceID); err != nil {
		writeError(w, 500, "could not lock relations")
		return
	}
	row, err := q.GetIssueDependency(r.Context(), db.GetIssueDependencyParams{ID: id, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "relation not found")
		return
	}
	if err != nil {
		writeError(w, 500, "could not load relation")
		return
	}
	if row.Type != "related" && (actorType != "member" || isMachineCredentialActor(r)) {
		writeError(w, 403, "execution dependency changes require human credentials")
		return
	}
	dependent, prerequisite := dependencyEndpoints(row)
	otherID := row.DependsOnIssueID
	if otherID == issue.ID {
		otherID = row.IssueID
	}
	other, err := q.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: otherID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, 404, "relation not found")
		return
	}
	if row.RemovedAt.Valid {
		if accept {
			writeError(w, 409, "relation was removed")
		} else {
			writeJSON(w, 200, map[string]bool{"removed": true})
		}
		return
	}
	if accept {
		var condition issueworkflow.DependencyCondition
		if json.Unmarshal(row.Condition, &condition) != nil || condition.Kind != "accepted" {
			writeError(w, 400, "this relation does not require explicit acceptance")
			return
		}
		var req struct {
			Revision        *int64 `json:"revision"`
			EvidenceVersion string `json:"evidence_version"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req) != nil || req.Revision == nil || !provenanceHash.MatchString(req.EvidenceVersion) {
			writeError(w, 400, "source revision and evidence_version are required")
			return
		}
		source, err := q.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: prerequisite, WorkspaceID: issue.WorkspaceID})
		if err != nil || int64(source.Revision) != *req.Revision {
			writeError(w, 409, "source changed; reload before accepting")
			return
		}
		currentVersion, versionErr := q.IssueEvidenceVersion(r.Context(), source.ID)
		if versionErr != nil {
			writeError(w, 500, "could not check source evidence")
			return
		}
		if currentVersion != req.EvidenceVersion {
			writeError(w, 409, "source evidence changed; reload before accepting")
			return
		}
		if source.Status != dependencyDeliveryStep(condition) && issuestatus.Category(r.Context(), q, source.WorkspaceID, source.Status) != "done" {
			writeError(w, 409, "source has not been delivered")
			return
		}
		var prior struct {
			Revision        int64  `json:"accepted_revision"`
			EvidenceVersion string `json:"accepted_evidence_version"`
		}
		if json.Unmarshal(row.Provenance, &prior) == nil && prior.Revision == *req.Revision && prior.EvidenceVersion == req.EvidenceVersion {
			writeJSON(w, 200, map[string]bool{"accepted": true, "removed": false})
			return
		}
		acceptance, _ := json.Marshal(map[string]any{"accepted_by": actorID, "accepted_revision": source.Revision, "accepted_evidence_version": req.EvidenceVersion, "accepted_at": time.Now().UTC()})
		_, err = q.AcceptIssueDependency(r.Context(), db.AcceptIssueDependencyParams{ID: row.ID, Acceptance: acceptance})
	} else {
		if row.Type != "related" && !h.dependencyEditable(w, r, q, dependent) {
			return
		}
		removal, _ := json.Marshal(map[string]string{"removed_by_type": actorType, "removed_by_id": actorID})
		_, err = q.RemoveIssueDependency(r.Context(), db.RemoveIssueDependencyParams{ID: row.ID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, Removal: removal})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "could not update relation")
		return
	}
	h.publishIssueRelationChanges(issue, other)
	writeJSON(w, 200, map[string]bool{"accepted": accept, "removed": !accept})
}

func (h *Handler) publishIssueRelationChanges(a, b db.Issue) {
	ids := []string{uuidToString(a.ID)}
	if a.ID != b.ID {
		ids = append(ids, uuidToString(b.ID))
	}
	h.publish(protocol.EventIssueRelationsChanged, uuidToString(a.WorkspaceID), "system", "", map[string]any{"issue_ids": ids})
}

func (h *Handler) dependencyIssueIdentifier(ctx context.Context, issue db.Issue) string {
	return fmt.Sprintf("%s-%d", h.getIssuePrefix(ctx, issue.WorkspaceID), issue.Number)
}

func dependencyDeliveryStep(c issueworkflow.DependencyCondition) string {
	if c.SourceStep != "" {
		return c.SourceStep
	}
	return "in_review"
}

var provenanceHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func provenanceEntryStep(raw []byte) string {
	var snapshot struct {
		Step string `json:"workflow_entry_step"`
	}
	_ = json.Unmarshal(raw, &snapshot)
	return snapshot.Step
}
