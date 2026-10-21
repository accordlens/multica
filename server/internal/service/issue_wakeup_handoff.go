package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ReviewHandoffCondition is one explicit, finite subscription to a source
// revision in an existing task thread. The wakeup's agent is the recipient.
type ReviewHandoffCondition struct {
	ThreadID        string `json:"thread_id"`
	SenderID        string `json:"sender_id"`
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion string `json:"workflow_version"`
	SourceRevision  string `json:"source_revision"`
}

var errHandoffSourceChanged = errors.New("handoff source or workflow changed; inspect the task and register a subscription for the current revision")

func validateHandoffCondition(ctx context.Context, tx pgx.Tx, issue db.Issue, c *ReviewHandoffCondition) error {
	if issue.AssigneeType.String == "squad" {
		return badCondition("the scoped review pilot does not support squad-owned conversations")
	}
	if c == nil || !issueworkflow.IsSourceRevision(c.SourceRevision) || !issueworkflow.IsDefinitionVersion(c.WorkflowVersion) {
		return badCondition("handoff requires exact source and workflow revisions")
	}
	for _, s := range []string{c.ThreadID, c.SenderID, c.WorkflowID} {
		id, err := util.ParseUUID(s)
		if err != nil || !id.Valid || util.UUIDToString(id) != s || s == "00000000-0000-0000-0000-000000000000" {
			return badCondition("handoff requires canonical thread, sender and workflow UUIDs")
		}
	}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment c JOIN agent a ON a.workspace_id=c.workspace_id
		WHERE c.id=$1 AND c.issue_id=$2 AND c.workspace_id=$3 AND c.parent_id IS NULL
		AND c.deleted_at IS NULL AND a.id=$4 AND a.archived_at IS NULL)`, c.ThreadID, issue.ID, issue.WorkspaceID, c.SenderID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return badCondition("handoff source is unavailable in this issue")
	}
	match, err := handoffWorkflowMatches(ctx, tx, issue.ID, issue.WorkspaceID, *c)
	if err != nil {
		return err
	}
	if !match {
		return badCondition("handoff workflow version does not match this project's current definition")
	}
	return nil
}

func handoffWorkflowMatches(ctx context.Context, tx pgx.Tx, issue, workspace pgtype.UUID, c ReviewHandoffCondition) (bool, error) {
	var steps []byte
	err := tx.QueryRow(ctx, `SELECT w.steps FROM issue i JOIN project p ON p.id=i.project_id AND p.workspace_id=i.workspace_id
		JOIN issue_workflow w ON w.id=p.workflow_id AND w.workspace_id=i.workspace_id
		WHERE i.id=$1 AND i.workspace_id=$2 AND w.id=$3 AND COALESCE(i.assignee_type,'')<>'squad'`, issue, workspace, c.WorkflowID).Scan(&steps)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	version, err := issueworkflow.DefinitionVersion(steps)
	return version == c.WorkflowVersion, err
}

func handoffSubscriptionInput(in *WakeupInput, c *ReviewHandoffCondition) error {
	target, err := util.ParseUUID(in.AgentID)
	if err != nil || !target.Valid || util.UUIDToString(target) != in.AgentID || in.AgentID == "00000000-0000-0000-0000-000000000000" {
		return badCondition("handoff recipient must be a canonical agent UUID")
	}
	if c == nil || c.SenderID == in.AgentID || in.Mode != "once" || in.OnTimeout == "wake" ||
		(in.ExpiresAt == nil && in.ExpiresInSeconds == 0) || in.ExpiresInSeconds > 72*3600 {
		return badCondition("handoff requires two roles, mode once, a deadline within 72h, and no timeout invocation")
	}
	if in.ParentCommentID != "" && in.ParentCommentID != c.ThreadID {
		return badCondition("handoff reply parent must be its topic thread")
	}
	in.ParentCommentID = c.ThreadID
	return nil
}

func evaluateHandoffCondition(ctx context.Context, tx pgx.Tx, q *db.Queries, w db.IssueWakeup, c *ReviewHandoffCondition) (bool, string, map[string]any, error) {
	if c == nil {
		return false, "", nil, errHandoffSourceChanged
	}
	match, err := handoffWorkflowMatches(ctx, tx, w.IssueID, w.WorkspaceID, *c)
	if err != nil || !match {
		if err == nil {
			err = errHandoffSourceChanged
		}
		return false, "", nil, err
	}
	senderID, err := util.ParseUUID(c.SenderID)
	if err != nil {
		return false, "", nil, errHandoffSourceChanged
	}
	sender, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: senderID, WorkspaceID: w.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (sender.ArchivedAt.Valid || !CanMemberInvokeAgent(ctx, q, sender, w.CreatedBy, w.WorkspaceID))) {
		return false, "", nil, errHandoffSourceChanged
	}
	if err != nil {
		return false, "", nil, err
	}
	// Direct replies keep the scan bounded and the topic explicit. A deleted
	// root removes the whole topic from routing without copying its contents.
	rows, err := tx.Query(ctx, `SELECT c.id,c.content,c.source_task_id,c.created_at FROM comment c
		JOIN comment root ON root.id=c.parent_id AND root.issue_id=c.issue_id AND root.workspace_id=c.workspace_id
		WHERE c.issue_id=$1 AND c.workspace_id=$2 AND c.parent_id=$3 AND c.author_type='agent'
		AND c.author_id=$4 AND c.deleted_at IS NULL AND root.deleted_at IS NULL
		AND octet_length(c.content)<=12100 AND btrim(c.content,E' \t\n\r') LIKE $5 ORDER BY c.created_at DESC,c.id DESC LIMIT 101`,
		w.IssueID, w.WorkspaceID, c.ThreadID, c.SenderID, strings.TrimSuffix(issueworkflow.HandoffFence, "\n")+"%")
	if err != nil {
		return false, "", nil, err
	}
	type candidate struct {
		id, run pgtype.UUID
		content string
		at      time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.id, &item.content, &item.run, &item.at); err != nil {
			rows.Close()
			return false, "", nil, err
		}
		candidates = append(candidates, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, "", nil, err
	}
	if len(candidates) > 100 {
		return false, "", nil, fmt.Errorf("%w: topic exceeds 100 handoff records; start a new scoped thread", errHandoffSourceChanged)
	}
	for _, item := range candidates {
		e, decodeErr := issueworkflow.DecodeHandoff(item.content)
		if decodeErr != nil || e.Type != "review.request" ||
			e.IssueID != util.UUIDToString(w.IssueID) || e.ThreadID != c.ThreadID || e.SenderID != c.SenderID ||
			e.RecipientID != util.UUIDToString(w.AgentID) || e.WorkflowID != c.WorkflowID || e.WorkflowVersion != c.WorkflowVersion ||
			e.SourceRevision != c.SourceRevision || !item.run.Valid || e.RunID != util.UUIDToString(item.run) {
			continue
		}
		run, err := q.GetAgentTask(ctx, item.run)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, "", nil, err
		}
		if run.IssueID != w.IssueID || run.OriginatorUserID != w.CreatedBy || util.UUIDToString(run.AgentID) != c.SenderID ||
			!run.WorkflowStep.Valid || run.WorkflowStep.String != e.WorkflowStep ||
			(run.Status != "running" && run.Status != "completed") || e.OccurredAt.After(item.at.Add(5*time.Minute)) {
			continue
		}
		available := true
		for _, artifact := range e.Artifacts {
			var exists bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attachment a
				WHERE a.id=$1 AND a.workspace_id=$2 AND a.issue_id=$3 AND a.source_context_id IS NULL
				AND a.chat_session_id IS NULL AND (a.comment_id IS NULL OR EXISTS(SELECT 1 FROM comment ac
				WHERE ac.id=a.comment_id AND ac.issue_id=$3 AND ac.workspace_id=$2 AND ac.deleted_at IS NULL)))`,
				artifact.AttachmentID, w.WorkspaceID, w.IssueID).Scan(&exists)
			if err != nil {
				return false, "", nil, err
			}
			available = available && exists
		}
		if !available {
			continue
		}
		var now time.Time
		if err := tx.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
			return false, "", nil, err
		}
		age := max(int64(now.Sub(item.at).Seconds()), 0)
		return true, "handoff:" + e.ID, map[string]any{
			"handoff": e, "handoff_id": e.ID, "comment_id": util.UUIDToString(item.id), "thread_id": c.ThreadID, "source_task_id": e.RunID,
			"published_at": item.at.UTC().Format(time.RFC3339Nano), "age_seconds": age, "delayed": age >= 60,
		}, nil
	}
	return false, "", nil, nil
}

func handoffCondition(raw []byte) *ReviewHandoffCondition {
	var c WakeupCondition
	if json.Unmarshal(raw, &c) == nil && c.Type == "review_handoff" {
		return c.Handoff
	}
	return nil
}

func isReviewHandoff(raw []byte) bool {
	var c WakeupCondition
	return json.Unmarshal(raw, &c) == nil && c.Type == "review_handoff"
}

func (s *IssueWakeupService) checkHandoffClaim(ctx context.Context, task db.AgentTaskQueue, w db.IssueWakeup) error {
	if w.ExpiresAt.Valid && !w.ExpiresAt.Time.After(time.Now()) {
		return ErrWakeupForbidden
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	met, fingerprint, observed, err := evaluateHandoffCondition(ctx, tx, s.Tasks.Queries.WithTx(tx), w, handoffCondition(w.Condition))
	if err != nil && !conditionDiagnostic(err) {
		return err
	}
	if !met {
		return ErrWakeupForbidden
	}
	// Do not substitute a newer request into a prompt that was already frozen.
	var evidence struct {
		Evidence wakeupEvidence `json:"wakeup_evidence"`
	}
	if json.Unmarshal(task.Context, &evidence) != nil {
		return ErrWakeupForbidden
	}
	current, _ := json.Marshal(observed["handoff"])
	for _, fact := range evidence.Evidence.Facts {
		var payload struct {
			Observed struct {
				HandoffID string          `json:"handoff_id"`
				Handoff   json.RawMessage `json:"handoff"`
			} `json:"observed"`
		}
		if json.Unmarshal(fact.Payload, &payload) == nil && fingerprint == "handoff:"+payload.Observed.HandoffID &&
			string(canonicalWakeupPayload(current)) == string(canonicalWakeupPayload(payload.Observed.Handoff)) {
			return nil
		}
	}
	return ErrWakeupForbidden
}
