package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

var issueURLReference = regexp.MustCompile(`(?i)(?:mention://issue/|/issues/)([a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12})(?:\b|/)`)

// Sync is also an explicit recovery operation after an interrupted description
// write. Removed links are tombstones: replaying the same source never restores
// a person's correction. Text references only create context, never blockers.
func (h *Handler) SyncIssueContextLinks(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadDependencyIssue(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	count, err := h.syncIssueContextLinks(r.Context(), issue)
	if err != nil {
		writeError(w, 500, "could not sync context links")
		return
	}
	writeJSON(w, 200, map[string]int{"created": count})
}

func (h *Handler) syncIssueContextLinks(ctx context.Context, issue db.Issue) (int, error) {
	refs := extractIdentifiers(issue.Description.String)
	urls := issueURLReference.FindAllStringSubmatch(issue.Description.String, 100)
	if len(refs) > 100 {
		refs = refs[:100]
	}
	if len(refs) == 0 && len(urls) == 0 {
		return 0, nil
	}
	others := map[string]db.Issue{}
	for _, ref := range refs {
		if other, ok := h.resolveIssueByIdentifier(ctx, ref, uuidToString(issue.WorkspaceID)); ok && other.ID != issue.ID {
			others[uuidToString(other.ID)] = other
		}
	}
	for _, match := range urls {
		id, err := util.ParseUUID(match[1])
		if err != nil {
			continue
		}
		other, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
		if err == nil && other.ID != issue.ID {
			others[uuidToString(other.ID)] = other
		}
	}
	if len(others) == 0 {
		return 0, nil
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if err = q.LockIssueDependencyGraph(ctx, issue.WorkspaceID); err != nil {
		return 0, err
	}
	rows, err := q.ListIssueDependencies(ctx, db.ListIssueDependenciesParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, IncludeRemoved: true})
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if row.Type != "related" {
			continue
		}
		other := row.DependsOnIssueID
		if other == issue.ID {
			other = row.IssueID
		}
		delete(others, uuidToString(other))
	}
	created := []db.Issue{}
	for _, other := range others {
		provenance, _ := json.Marshal(map[string]any{"source": "description", "source_issue_id": uuidToString(issue.ID), "source_revision": issue.Revision})
		_, err = q.CreateIssueDependency(ctx, db.CreateIssueDependencyParams{ID: dbid.NewV7(), IssueID: issue.ID, DependsOnIssueID: other.ID, Type: "related", Condition: []byte(`{"kind":"done"}`), Provenance: provenance})
		if err != nil {
			return 0, err
		}
		created = append(created, other)
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	for _, other := range created {
		h.publishIssueRelationChanges(issue, other)
	}
	return len(created), nil
}
