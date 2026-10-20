package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type reviewPilot struct {
	f                      principalFixture
	s                      *IssueWakeupService
	issue                  pgtype.UUID
	dev, qa, run, artifact string
	filter                 ReviewHandoffCondition
}

func newReviewPilot(t *testing.T) reviewPilot {
	t.Helper()
	f, s, issue, qa := conditionFixture(t)
	dev := f.privateAgentOwnedBy(t, f.UserID, "handoff-dev")
	steps, err := issueworkflow.EncodeSteps(issueworkflow.Arrange([]issueworkflow.Step{
		{StatusKey: "in_progress", Handler: issueworkflow.Handler{Type: "agent", ID: dev}},
		{StatusKey: "in_review", Handler: issueworkflow.Handler{Type: "agent", ID: qa}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	workflow := f.Insert(t, "issue_workflow", testutil.Cols{"workspace_id": f.WorkspaceID, "name": "Local review pilot", "initial_status_key": "todo", "steps": steps})
	project := f.Project(t, "Local review pilot", testutil.Cols{"workflow_id": workflow})
	f.Exec(t, "UPDATE issue SET project_id=$2 WHERE id=$1", issue, project)
	version, err := issueworkflow.DefinitionVersion(steps)
	if err != nil {
		t.Fatal(err)
	}
	root := f.Comment(t, util.UUIDToString(issue), "Dev and QA: review one pinned revision. Expected response: review.result. Decision pending.")
	run := f.Task(t, dev, testutil.Cols{"issue_id": issue, "status": "completed", "workflow_step": "in_progress",
		"originator_user_id": f.UserID, "accountable_user_id": f.UserID,
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + dev + "')")})
	artifact := f.Insert(t, "attachment", testutil.Cols{"workspace_id": f.WorkspaceID, "issue_id": issue,
		"uploader_type": "agent", "uploader_id": dev, "filename": "synthetic-report.json",
		"url": "http://127.0.0.1/synthetic-not-served", "content_type": "application/json", "size_bytes": 2})
	return reviewPilot{f: f, s: s, issue: issue, dev: dev, qa: qa, run: run, artifact: artifact,
		filter: ReviewHandoffCondition{ThreadID: root, SenderID: dev, WorkflowID: workflow, WorkflowVersion: version, SourceRevision: strings.Repeat("a", 40)}}
}

func (p reviewPilot) input() WakeupInput {
	raw, _ := json.Marshal(WakeupCondition{Type: "review_handoff", Handoff: &p.filter})
	return WakeupInput{AgentID: p.qa, Kind: "event", Mode: "once", ExpiresInSeconds: 3600,
		Instruction: "Review only the pinned source revision. Read the source report. Return review.result in this thread; do not advance the workflow.", Condition: raw}
}

func (p reviewPilot) envelope() issueworkflow.HandoffEnvelope {
	return issueworkflow.HandoffEnvelope{Version: 1, ID: util.UUIDToString(dbid.NewV7()), Type: "review.request",
		SenderID: p.dev, RecipientID: p.qa, IssueID: util.UUIDToString(p.issue), ThreadID: p.filter.ThreadID,
		RunID: p.run, WorkflowID: p.filter.WorkflowID, WorkflowVersion: p.filter.WorkflowVersion, WorkflowStep: "in_progress",
		SourceRevision: p.filter.SourceRevision, OccurredAt: time.Now().UTC(), Visibility: "issue", Goal: "Review this source revision.",
		Summary: "Synthetic report attached.", Uncertainty: "No model execution; report is a fixture.", ExpectedResponse: "review.result",
		Artifacts: []issueworkflow.HandoffArtifact{{AttachmentID: p.artifact, SHA256: strings.Repeat("b", 64)}}}
}

func (p reviewPilot) publish(t *testing.T, e issueworkflow.HandoffEnvelope, over ...testutil.Cols) string {
	t.Helper()
	content, err := issueworkflow.EncodeHandoff(e)
	if err != nil {
		t.Fatal(err)
	}
	cols := testutil.Cols{"id": e.ID, "parent_id": p.filter.ThreadID, "author_type": "agent", "author_id": e.SenderID, "source_task_id": e.RunID}
	for _, overrides := range over {
		for k, v := range overrides {
			cols[k] = v
		}
	}
	return p.f.Comment(t, util.UUIDToString(p.issue), content, cols)
}

func TestReviewHandoffPilotOfflineReplayAndDecision(t *testing.T) {
	p := newReviewPilot(t)
	ctx := context.Background()
	w := wakeCreate(t, p.f, p.s, p.issue, p.input())
	// Both roles explicitly subscribe to requests. Results and decision changes
	// do not implicitly ask the other role to work.
	back := p
	back.qa, back.dev = p.dev, p.qa
	back.filter.SenderID = p.qa
	d := wakeCreate(t, p.f, p.s, p.issue, back.input())
	for range 20 {
		p.f.Comment(t, util.UUIDToString(p.issue), "Human discussion", testutil.Cols{"parent_id": p.filter.ThreadID})
	}
	wakeTick(t, p.f, p.s, w.ID)
	wakeTick(t, p.f, p.s, d.ID)
	if n := wakeRuns(t, p.f, w.ID) + wakeRuns(t, p.f, d.ID); n != 0 {
		t.Fatalf("20 human messages started %d runs", n)
	}
	notice := p.envelope()
	notice.Type, notice.ExpectedResponse = "decision.changed", "none"
	p.publish(t, notice)
	wakeTick(t, p.f, p.s, w.ID)
	if n := wakeRuns(t, p.f, w.ID); n != 0 {
		t.Fatalf("decision notice started %d runs", n)
	}

	e := p.envelope()
	e.OccurredAt = time.Now().UTC().Add(-2 * time.Minute)
	p.publish(t, e, testutil.Cols{"created_at": testutil.Raw("now()-interval '2 minutes'")})
	if n := p.f.Count(t, "SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL", w.ID); n == 0 {
		t.Fatal("offline source event was not durably captured")
	}
	receipts, err := p.f.q.ListPendingWakeupReceipts(ctx, db.ListPendingWakeupReceiptsParams{WakeupID: w.ID, Revision: w.Revision})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range receipts {
		if _, err := p.f.q.RecordWakeupReceipt(ctx, db.RecordWakeupReceiptParams{ID: dbid.NewV7(), WakeupID: w.ID, Revision: w.Revision, EventKey: r.EventKey, EventType: r.EventType, Payload: r.Payload}); err != nil {
			t.Fatal(err)
		}
	}
	// Reconstruct the service from PostgreSQL, with no in-memory checkpoint.
	restarted := &IssueWakeupService{Tasks: &TaskService{Queries: db.New(p.f.Pool), TxStarter: p.f.Pool, Bus: events.New()}}
	start := time.Now()
	drainTick(t, restarted, parseTestUUID(t, p.f.WorkspaceID))
	drainTick(t, restarted, parseTestUUID(t, p.f.WorkspaceID))
	dispatchDuration := time.Since(start)
	w, err = p.f.q.LocklessWakeup(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := wakeRuns(t, p.f, w.ID); n != 1 {
		t.Fatalf("reconnect/replay produced %d logical handoffs", n)
	}
	task, err := p.f.q.GetAgentTask(ctx, w.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.AgentID != parseTestUUID(t, p.qa) || util.UUIDToString(task.TriggerCommentID) != p.filter.ThreadID ||
		!strings.Contains(task.HandoffNote.String, e.SourceRevision) || !strings.Contains(task.HandoffNote.String, `"delayed":true`) ||
		!strings.Contains(task.HandoffNote.String, e.RunID) || !strings.Contains(task.HandoffNote.String, p.artifact) {
		t.Fatalf("incomplete handoff: %s", task.HandoffNote.String)
	}
	if err := restarted.CheckClaim(ctx, task); err != nil {
		t.Fatalf("offline claim: %v", err)
	}
	// This is the fake provider boundary: no executable or paid model is used.
	p.f.Exec(t, "UPDATE agent_task_queue SET status='running',started_at=now() WHERE id=$1", task.ID)
	result := p.envelope()
	result.Type, result.ExpectedResponse, result.Decision = "review.result", "none", "approve"
	result.SenderID, result.RecipientID, result.RunID = p.qa, p.dev, util.UUIDToString(task.ID)
	result.WorkflowStep = task.WorkflowStep.String
	result.CausationID = e.ID
	result.Summary = "Synthetic review complete; no workflow gate is satisfied by this statement."
	resultID := p.publish(t, result)
	// The pilot uses the existing issue metadata index, carrying references
	// rather than copied private content. It is not a dependency result.
	index, _ := json.Marshal(map[string]any{"version": 1, "thread_id": p.filter.ThreadID, "comment_id": resultID, "run_id": result.RunID,
		"source_revision": result.SourceRevision, "workflow_version": result.WorkflowVersion, "decision": result.Decision, "artifacts": result.Artifacts})
	if _, err := p.f.q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{ID: p.issue, WorkspaceID: parseTestUUID(t, p.f.WorkspaceID), Key: "handoff_index_v1", Value: index}); err != nil {
		t.Fatal(err)
	}
	p.f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1", task.ID)
	drainTick(t, restarted, parseTestUUID(t, p.f.WorkspaceID))
	if n := wakeRuns(t, p.f, w.ID) + wakeRuns(t, p.f, d.ID); n != 1 {
		t.Fatalf("terminal result caused %d total runs", n)
	}
	issue, err := p.f.q.GetIssue(ctx, p.issue)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Status != "todo" || !strings.Contains(string(issue.Metadata), resultID) {
		t.Fatalf("decision lost or changed workflow status: %s %s", issue.Status, issue.Metadata)
	}
	t.Logf("ACCO-96 pilot: human_messages=20 human_runs=0 logical_handoffs=1 duplicate_work=0 decision_indexed=true model_calls=0 routing_tokens=0 reconnect_to_queue_ms=%.3f offline_delay_seconds>=120", float64(dispatchDuration.Microseconds())/1000)
}

func TestReviewHandoffRejectsStaleForgedAndForeignSources(t *testing.T) {
	for _, name := range []string{"wrong revision", "wrong recipient", "wrong source run", "foreign artifact", "wrong author", "old definition", "missing report", "unattributed source", "squad routing"} {
		t.Run(name, func(t *testing.T) {
			p := newReviewPilot(t)
			w := wakeCreate(t, p.f, p.s, p.issue, p.input())
			e := p.envelope()
			over := testutil.Cols{}
			switch name {
			case "wrong revision":
				e.SourceRevision = strings.Repeat("c", 40)
			case "wrong recipient":
				e.RecipientID = util.UUIDToString(dbid.NewV7())
			case "wrong source run":
				e.RunID = util.UUIDToString(dbid.NewV7())
			case "foreign artifact":
				other := p.f.Issue(t, "Other task")
				p.f.Exec(t, "UPDATE attachment SET issue_id=$2 WHERE id=$1", p.artifact, other)
			case "wrong author":
				over["author_id"] = p.qa
			case "old definition":
				p.f.Exec(t, "UPDATE issue_workflow SET steps='[]'::jsonb WHERE id=$1", p.filter.WorkflowID)
			case "missing report":
				p.f.Exec(t, "DELETE FROM attachment WHERE id=$1", p.artifact)
			case "unattributed source":
				p.f.Exec(t, "UPDATE agent_task_queue SET originator_user_id=NULL WHERE id=$1", p.run)
			case "squad routing":
				p.f.Exec(t, "UPDATE issue SET assignee_type='squad',assignee_id=$2 WHERE id=$1", p.issue, p.dev)
			}
			p.publish(t, e, over)
			wakeTick(t, p.f, p.s, w.ID)
			if n := wakeRuns(t, p.f, w.ID); n != 0 {
				t.Fatalf("invalid source started %d runs", n)
			}
		})
	}
}

func TestReviewHandoffRechecksRevocationAndFrozenEvidenceAtClaim(t *testing.T) {
	for _, name := range []string{"unsubscribe", "membership", "artifact removed", "source edit", "workflow edit", "deadline", "sender access"} {
		t.Run(name, func(t *testing.T) {
			p := newReviewPilot(t)
			w := wakeCreate(t, p.f, p.s, p.issue, p.input())
			e := p.envelope()
			p.publish(t, e)
			w = wakeTick(t, p.f, p.s, w.ID)
			task, err := p.f.q.GetAgentTask(context.Background(), w.LastTaskID)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "unsubscribe":
				err = p.s.Delete(context.Background(), p.issue, w.ID, parseTestUUID(t, p.f.UserID))
			case "membership":
				p.f.Exec(t, "DELETE FROM member WHERE workspace_id=$1 AND user_id=$2", p.f.WorkspaceID, p.f.UserID)
			case "artifact removed":
				p.f.Exec(t, "DELETE FROM attachment WHERE id=$1", p.artifact)
			case "source edit":
				e.Summary = "Edited after dispatch"
				content, _ := issueworkflow.EncodeHandoff(e)
				p.f.Exec(t, "UPDATE comment SET content=$2 WHERE id=$1", e.ID, content)
			case "workflow edit":
				p.f.Exec(t, "UPDATE issue_workflow SET steps='[]'::jsonb WHERE id=$1", p.filter.WorkflowID)
			case "deadline":
				p.f.Exec(t, "UPDATE issue_wakeup SET expires_at=now()-interval '1 minute' WHERE id=$1", w.ID)
			case "sender access":
				p.f.Exec(t, "UPDATE agent SET archived_at=now() WHERE id=$1", p.dev)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := p.s.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
				t.Fatalf("revocation did not block claim: %v", err)
			}
		})
	}
}

func TestReviewHandoffHasOneSubscriptionAndFiniteBudget(t *testing.T) {
	p := newReviewPilot(t)
	w := wakeCreate(t, p.f, p.s, p.issue, p.input())
	if _, err := p.s.Create(context.Background(), p.issue, parseTestUUID(t, p.f.UserID), pgtype.UUID{}, p.input()); !errors.Is(err, ErrWakeupConflict) {
		t.Fatalf("duplicate subscription accepted: %v", err)
	}
	for _, name := range []string{"continuous", "no deadline", "timeout invocation", "self", "different parent"} {
		in := p.input()
		switch name {
		case "continuous":
			in.Mode = "continuous"
		case "no deadline":
			in.ExpiresInSeconds = 0
		case "timeout invocation":
			in.OnTimeout = "wake"
		case "self":
			in.AgentID = p.dev
		case "different parent":
			in.ParentCommentID = util.UUIDToString(dbid.NewV7())
		}
		if _, err := p.s.Create(context.Background(), p.issue, parseTestUUID(t, p.f.UserID), pgtype.UUID{}, in); !errors.Is(err, ErrWakeupInput) {
			t.Fatalf("%s subscription accepted: %v", name, err)
		}
	}
	for range 5 {
		p.publish(t, p.envelope())
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- p.s.dispatch(context.Background(), w) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	finishWakeupRuns(t, p.f, w.ID)
	for range 5 {
		p.publish(t, p.envelope())
		wakeTick(t, p.f, p.s, w.ID)
	}
	if n := wakeRuns(t, p.f, w.ID); n != 1 {
		t.Fatalf("finite subscription produced %d runs", n)
	}
	if err := p.s.Trigger(context.Background(), p.issue, w.ID, parseTestUUID(t, p.f.UserID)); !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("wake-now bypassed handoff: %v", err)
	}
}

func TestReviewHandoffKeepsItsScopeBesideOtherQueuedWork(t *testing.T) {
	p := newReviewPilot(t)
	w := wakeCreate(t, p.f, p.s, p.issue, p.input())
	ordinary := wakeWaitingRun(t, p.f, p.issue, p.qa, p.f.UserID)
	p.publish(t, p.envelope())
	wakeTick(t, p.f, p.s, w.ID)
	if n := wakeRuns(t, p.f, w.ID); n != 0 {
		t.Fatalf("handoff raced existing queued work: %d", n)
	}
	if note := wakeClaim(t, p.f, p.s, ordinary); strings.Contains(note, "review_handoff") {
		t.Fatal("pinned source evidence entered an unrelated run")
	}
	wakeStart(t, p.f, ordinary)
	p.f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1", ordinary)
	wakeTick(t, p.f, p.s, w.ID)
	if n := wakeRuns(t, p.f, w.ID); n != 1 {
		t.Fatalf("handoff was not retained for its own run: %d", n)
	}
}

func TestReviewHandoffRetainsTheExistingCausalLoopGuard(t *testing.T) {
	p := newReviewPilot(t)
	w := wakeCreate(t, p.f, p.s, p.issue, p.input())
	chain, _ := json.Marshal(map[string]any{"wakeup_chain": []string{util.UUIDToString(w.ID), util.UUIDToString(w.ID)}})
	p.f.Exec(t, "UPDATE agent_task_queue SET context=$2 WHERE id=$1", p.run, chain)
	p.publish(t, p.envelope())
	w = wakeTick(t, p.f, p.s, w.ID)
	if w.Enabled || w.PausedReason.String != wakeupPausedLoop || wakeRuns(t, p.f, w.ID) != 0 {
		t.Fatalf("causal loop was not stopped: enabled=%t reason=%s", w.Enabled, w.PausedReason.String)
	}
}
