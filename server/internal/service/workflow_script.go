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
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type workflowCompletion struct {
	Output   string                  `json:"output"`
	Evidence *issueworkflow.Evidence `json:"workflow_evidence"`
}

func lockScriptCompletion(ctx context.Context, q *db.Queries, taskID pgtype.UUID) (*db.Issue, error) {
	task, err := q.GetAgentTask(ctx, taskID)
	if err != nil || !task.IssueID.Valid {
		return nil, err
	}
	agent, err := q.GetAgent(ctx, task.AgentID)
	if err != nil {
		return nil, err
	}
	issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: task.IssueID, WorkspaceID: agent.WorkspaceID})
	if err != nil {
		return nil, err
	}
	if issueworkflow.ParseExecution(task.Context) == nil {
		def, _, err := issueworkflow.ForProject(ctx, q, issue.WorkspaceID, issue.ProjectID)
		if err != nil {
			return nil, err
		}
		if def == nil || !def.HasScripts() {
			return nil, nil
		}
	}
	// Issue writes cancel task rows while owning the issue. Use the same
	// issue-before-task order for a completion that creates a successor.
	locked, err := q.LockWorkflowIssue(ctx, db.LockWorkflowIssueParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	return &locked, err
}

// advanceScriptWorkflow runs inside the source task's completion transaction.
// A lost response replays the existing completion, never this transition.
func (s *TaskService) advanceScriptWorkflow(ctx context.Context, q *db.Queries, task *db.AgentTaskQueue, successor *db.AgentTaskQueue) error {
	if !task.IssueID.Valid {
		return nil
	}
	agent, err := q.GetAgent(ctx, task.AgentID)
	if err != nil {
		return err
	}
	issue, err := q.LockWorkflowIssue(ctx, db.LockWorkflowIssueParams{ID: task.IssueID, WorkspaceID: agent.WorkspaceID})
	if err != nil {
		return err
	}
	def, _, err := issueworkflow.ForProject(ctx, q, issue.WorkspaceID, issue.ProjectID)
	if err != nil {
		return err
	}
	execution := issueworkflow.ParseExecution(task.Context)
	if execution == nil && (def == nil || !def.HasScripts()) {
		return nil
	}
	if !task.WorkflowStep.Valid || issue.Status != task.WorkflowStep.String || issue.Status == "cancelled" || issue.Status == "blocked" {
		return nil // A stale run cannot resurrect a step another writer moved.
	}
	var completion workflowCompletion
	_ = json.Unmarshal(task.Result, &completion)
	if execution == nil {
		// Only the current assignee's run may start the gate. Mentions and
		// observer runs do not acquire authority by sharing the issue UUID.
		if issue.AssigneeType.String != "agent" || issue.AssigneeID != task.AgentID {
			return nil
		}
		step, ok := def.Step(def.NextStatus(issue.Status))
		if current, found := def.Step(issue.Status); found && current.Script != nil {
			step, ok = current, true
		}
		if !ok || step.Script == nil {
			return nil
		}
		execution = &issueworkflow.Execution{
			Kind: "agent", RootTaskID: util.UUIDToString(task.ID), SourceTaskID: util.UUIDToString(task.ID),
			WorkflowID: util.UUIDToString(def.ID), WorkflowVersion: def.Version(), StepKey: step.StatusKey,
			AuthorID: util.UUIDToString(task.AgentID), AuthorRuntimeID: util.UUIDToString(task.RuntimeID),
			WorkDir: task.WorkDir.String, Script: *step.Script,
			Deadline: time.Now().UTC().Add(time.Duration(step.Script.BudgetSeconds) * time.Second),
		}
	}
	block := func(reason string) error {
		execution.BlockReason = reason
		raw, err := json.Marshal(execution)
		if err != nil {
			return err
		}
		updated, err := q.SetWorkflowTaskContext(ctx, db.SetWorkflowTaskContextParams{ID: task.ID, Execution: raw})
		if err != nil {
			return err
		}
		*task = updated
		_, err = q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: "blocked", SourceTaskID: task.ID})
		return err
	}
	if issue.AssigneeType.String != "agent" || issue.AssigneeID != task.AgentID {
		return block("workflow step ownership changed")
	}
	if !task.OriginatorUserID.Valid {
		return block("a script workflow requires an authorized human originator")
	}
	if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: task.OriginatorUserID, WorkspaceID: issue.WorkspaceID}); err != nil {
		return block("workflow originator is no longer authorized in this workspace")
	}
	if def == nil || def.Version() != execution.WorkflowVersion || util.UUIDToString(def.ID) != execution.WorkflowID {
		return block("workflow definition changed; a new execution and review are required")
	}
	if time.Now().After(execution.Deadline) {
		return block("workflow time budget exhausted")
	}
	step, ok := def.Step(execution.StepKey)
	if !ok || step.Script == nil || issueworkflow.Hash(*step.Script) != issueworkflow.Hash(execution.Script) {
		return block("script definition changed")
	}
	back, _ := def.Step(step.BackStatusKey)
	if back.Handler.ID != execution.AuthorID {
		return block("author no longer matches the configured repair step")
	}
	// A linked PR is authoritative when present. A partial merge or an
	// author's declaration cannot replace a check of the delivered head.
	prSHA, prErr := q.GetIssueReviewHeadSha(ctx, issue.ID)
	if prErr != nil && !errors.Is(prErr, pgx.ErrNoRows) {
		return prErr
	}
	evidence := completion.Evidence
	if evidence == nil || evidence.RunID != util.UUIDToString(task.ID) || evidence.WorkflowVersion != execution.WorkflowVersion || !evidence.Source.Clean() {
		return block("missing current clean source attestation from the runtime")
	}
	if prSHA != "" && prSHA != evidence.Source.SHA {
		return block("source SHA differs from the linked PR head")
	}
	switch execution.Kind {
	case "agent", "repair":
		if evidence.Outcome != "agent_completed" {
			return block("agent did not deliver a verified source revision")
		}
		if execution.Kind == "repair" && evidence.Source == execution.Source {
			return block("repair made no revision progress")
		}
		execution.Source = evidence.Source
		execution.Kind = "script"
		execution.SourceTaskID = util.UUIDToString(task.ID)
		execution.WorkDir = task.WorkDir.String
		return s.queueWorkflowSuccessor(ctx, q, issue, *task, execution, step.StatusKey, execution.AuthorID, execution.AuthorRuntimeID, "Run the frozen workflow script gate; do not invoke a model.", successor)
	case "script":
		if err := evidence.Validate(util.UUIDToString(task.ID), *execution, time.Now()); err != nil {
			return block(err.Error())
		}
		execution.Evidence = evidence
		execution.SourceTaskID = util.UUIDToString(task.ID)
		if evidence.Outcome == "assertion_failed" {
			if execution.Repairs >= execution.Script.MaxRepairs {
				return block("repair attempt limit exhausted")
			}
			authorID, parseErr := util.ParseUUID(execution.AuthorID)
			author, loadErr := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: authorID, WorkspaceID: issue.WorkspaceID})
			if parseErr != nil || loadErr != nil || author.ArchivedAt.Valid || !CanMemberInvokeAgent(ctx, q, author, task.OriginatorUserID, issue.WorkspaceID) {
				return block("repair invocation is no longer authorized")
			}
			execution.Repairs++
			execution.Kind = "repair"
			note := fmt.Sprintf("Repair attempt %d/%d for workflow %s. Fix the failed checks and commit a new revision. The engine reruns the gate automatically. Do not edit pinned gate files. Source SHA: %s. Report ID: %s; SHA256: %s.\nReport: %s\nLog:\n%s", execution.Repairs, execution.Script.MaxRepairs, execution.WorkflowVersion, evidence.Source.SHA, evidence.ReportID, evidence.ReportHash, evidence.Report, evidence.Log)
			return s.queueWorkflowSuccessor(ctx, q, issue, *task, execution, step.BackStatusKey, execution.AuthorID, execution.AuthorRuntimeID, note, successor)
		}
		reviewer, _ := def.Step(step.NextStatusKey)
		if reviewer.Handler.ID == execution.AuthorID {
			return block("author cannot review their own revision")
		}
		reviewerID, err := util.ParseUUID(reviewer.Handler.ID)
		if err != nil {
			return block("reviewer identity is invalid")
		}
		reviewAgent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: reviewerID, WorkspaceID: issue.WorkspaceID})
		if err != nil || reviewAgent.ArchivedAt.Valid || !reviewAgent.RuntimeID.Valid || !CanMemberInvokeAgent(ctx, q, reviewAgent, task.OriginatorUserID, issue.WorkspaceID) {
			return block("independent reviewer is unavailable or outside the authorized owner scope")
		}
		execution.Kind = "review"
		note := fmt.Sprintf("Independently review source SHA %s, workflow %s, script %s. Use a checkout of this exact revision. The author is %s. Report ID %s; SHA256 %s.\nReport: %s\nLog:\n%s\nEnd the result with exactly APPROVE %s, CHANGES_REQUESTED %s or INCOMPLETE %s. Human acceptance remains required.", evidence.Source.SHA, execution.WorkflowVersion, execution.Script.Version, execution.AuthorID, evidence.ReportID, evidence.ReportHash, evidence.Report, evidence.Log, evidence.Source.SHA, evidence.Source.SHA, evidence.Source.SHA)
		return s.queueWorkflowSuccessor(ctx, q, issue, *task, execution, step.NextStatusKey, reviewer.Handler.ID, util.UUIDToString(reviewAgent.RuntimeID), note, successor)
	case "review":
		if task.AgentID == mustWorkflowUUID(execution.AuthorID) || evidence.Source != execution.Source || execution.Evidence == nil || execution.Evidence.Outcome != "passed" {
			return block("review is not independent or source revision changed")
		}
		verdict := ""
		for _, candidate := range []string{"APPROVE", "CHANGES_REQUESTED", "INCOMPLETE"} {
			if strings.TrimSpace(completion.Output) == candidate+" "+execution.Source.SHA || strings.HasSuffix(strings.TrimSpace(completion.Output), "\n"+candidate+" "+execution.Source.SHA) {
				verdict = candidate
			}
		}
		if verdict != "APPROVE" {
			return block("review verdict: " + verdict + "; current independent APPROVE required")
		}
		execution.ReviewVerdict = verdict
		raw, _ := json.Marshal(execution)
		updated, err := q.SetWorkflowTaskContext(ctx, db.SetWorkflowTaskContextParams{ID: task.ID, Execution: raw})
		if err == nil {
			*task = updated
		}
		return err // Deliberately stays in review for human acceptance.
	default:
		return block("unknown workflow execution kind")
	}
}

func mustWorkflowUUID(value string) pgtype.UUID {
	id, _ := util.ParseUUID(value)
	return id
}

func (s *TaskService) queueWorkflowSuccessor(ctx context.Context, q *db.Queries, issue db.Issue, source db.AgentTaskQueue, execution *issueworkflow.Execution, status, agentID, runtimeID, note string, successor *db.AgentTaskQueue) error {
	agentUUID, err := util.ParseUUID(agentID)
	if err != nil {
		return err
	}
	runtimeUUID, err := util.ParseUUID(runtimeID)
	if err != nil {
		return err
	}
	_, err = q.MoveWorkflowIssue(ctx, db.MoveWorkflowIssueParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: status, AgentID: agentUUID, SourceTaskID: source.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		// advanceScriptWorkflow already locked this issue. An unmet dependency
		// declined the handoff: save the source completion and its evidence,
		// leaving the issue in its current step without a successor.
		return nil
	}
	if err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]any{"hybrid_workflow": execution, "head_sha": execution.Source.SHA})
	if err != nil {
		return err
	}
	created, err := q.CreateWorkflowSuccessor(ctx, db.CreateWorkflowSuccessorParams{ID: dbid.NewV7(), SourceID: source.ID, AgentID: agentUUID, RuntimeID: runtimeUUID, Context: raw, WorkflowStep: status, HandoffNote: note})
	if err == nil {
		*successor = created
	}
	return err
}

func settleScriptFailure(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) error {
	execution := issueworkflow.ParseExecution(task.Context)
	if execution == nil || task.Status == "completed" {
		return nil
	}
	agent, err := q.GetAgent(ctx, task.AgentID)
	if err != nil {
		return err
	}
	issue, err := q.LockWorkflowIssueForFailure(ctx, db.LockWorkflowIssueForFailureParams{ID: task.IssueID, WorkspaceID: agent.WorkspaceID})
	if err != nil {
		return err
	}
	if issue.Status != task.WorkflowStep.String || issue.Status == "cancelled" {
		return nil
	}
	execution.BlockReason = "workflow run " + task.Status + ": " + task.FailureReason.String + " " + task.Error.String + "; no automatic command replay"
	raw, _ := json.Marshal(execution)
	if _, err = q.SetWorkflowTaskContext(ctx, db.SetWorkflowTaskContextParams{ID: task.ID, Execution: raw}); err != nil {
		return err
	}
	_, err = q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: "blocked", SourceTaskID: task.ID})
	return err
}
