package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The external model is replaced by an asserted queue row. Comments,
// subscriptions, event capture, routing, claim authorization and reads use the
// application handlers and PostgreSQL, including workspace middleware.
func TestReviewHandoffHTTPEndToEnd(t *testing.T) {
	if testHandler == nil {
		t.Fatal("a migrated synthetic database is required")
	}
	ctx := context.Background()
	dev := dbfx.Agent(t, "handoff HTTP Dev", testRuntimeID, testutil.Cols{"owner_id": testUserID})
	qa := dbfx.Agent(t, "handoff HTTP QA", testRuntimeID, testutil.Cols{"owner_id": testUserID})
	wf := createTestWorkflowSteps(t, "Handoff "+workflowTestSuffix(), []map[string]any{
		{"status_key": "in_progress", "handler": map[string]string{"type": "agent", "id": dev}},
		{"status_key": "in_review", "handler": map[string]string{"type": "agent", "id": qa}},
	})
	if !issueworkflow.IsDefinitionVersion(wf.DefinitionVersion) {
		t.Fatal("workflow API did not expose its definition version")
	}
	project := dbfx.Project(t, "Handoff HTTP", testutil.Cols{"workflow_id": wf.ID})
	issue := dbfx.Issue(t, "Handoff HTTP", testutil.Cols{"project_id": project, "status": "in_progress"})
	dbfx.Cleanup(t, "DELETE FROM activity_log WHERE issue_id=$1", issue)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN(SELECT id FROM issue_wakeup WHERE issue_id=$1)", issue)
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", issue)
	post := func(content, parent, agent, run string) CommentResponse {
		t.Helper()
		body := map[string]any{"content": content}
		if parent != "" {
			body["parent_id"] = parent
		}
		req := withURLParam(newRequest("POST", "/api/issues/"+issue+"/comments", body), "id", issue)
		if agent != "" {
			req.Header.Set("X-Agent-ID", agent)
			req.Header.Set("X-Task-ID", run)
		}
		var result CommentResponse
		testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated).JSON(&result)
		return result
	}
	root := post("Dev and QA: review one revision. Expected response: review.result. Decision pending.", "", "", "")
	source := dbfx.Task(t, dev, testutil.Cols{"issue_id": issue, "runtime_id": testRuntimeID, "status": "running",
		"workflow_step": "in_progress", "originator_user_id": testUserID, "accountable_user_id": testUserID, "started_at": testutil.Raw("now()")})
	artifact := unlinkedIssueAttachment(t, issue)
	e := issueworkflow.HandoffEnvelope{Version: 1, ID: uuidToString(dbid.NewV7()), Type: "review.request",
		SenderID: dev, RecipientID: qa, IssueID: issue, ThreadID: root.ID, RunID: source, WorkflowID: wf.ID,
		WorkflowVersion: wf.DefinitionVersion, WorkflowStep: "in_progress", SourceRevision: strings.Repeat("a", 40),
		OccurredAt: time.Now().UTC(), Visibility: "issue", Goal: "Review this revision.", Summary: "Read the attached synthetic report.",
		Uncertainty: "No external model is called.", ExpectedResponse: "review.result",
		Artifacts: []issueworkflow.HandoffArtifact{{AttachmentID: artifact, SHA256: strings.Repeat("b", 64)}}}
	filter := service.ReviewHandoffCondition{ThreadID: root.ID, SenderID: dev, WorkflowID: wf.ID, WorkflowVersion: wf.DefinitionVersion, SourceRevision: e.SourceRevision}
	var rule db.IssueWakeup
	req := withURLParam(newRequest("POST", "/api/issues/"+issue+"/wakeups", map[string]any{
		"agent_id": qa, "instruction": "Review this revision and report in the source thread.", "kind": "event", "expires_in_seconds": 3600,
		"condition": map[string]any{"type": "review_handoff", "handoff": filter},
	}), "id", issue)
	testutil.Call(t, testHandler.CreateIssueWakeup, req).Want(http.StatusCreated).JSON(&rule)
	svc := &service.IssueWakeupService{Tasks: testHandler.TaskService}
	beforeHuman := dbfx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", issue)
	for range 20 {
		post("Human-only discussion", root.ID, "", "")
	}
	if err := svc.TickWorkspaces(ctx, parseUUID(testWorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2", issue, qa); n != 0 {
		t.Fatalf("20 human comments started %d reviewer runs", n)
	}
	if afterHuman := dbfx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", issue); afterHuman != beforeHuman {
		t.Fatalf("human-only thread started %d runs", afterHuman-beforeHuman)
	}
	content, err := issueworkflow.EncodeHandoff(e)
	if err != nil {
		t.Fatal(err)
	}
	content = strings.ReplaceAll(content, "\n", "\r\n") + "\r\n"
	request := post(content, root.ID, dev, source)
	if request.ID == e.ID {
		t.Fatal("producer event UUID must not require choosing the server's comment UUID")
	}
	post(content, root.ID, dev, source)
	if err := svc.TickWorkspaces(ctx, parseUUID(testWorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if err := svc.TickWorkspaces(ctx, parseUUID(testWorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2", issue, qa); n != 1 {
		t.Fatalf("duplicate publish produced %d reviewer runs", n)
	}
	current, err := testHandler.Queries.GetIssueWakeup(ctx, db.GetIssueWakeupParams{ID: rule.ID, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.Queries.GetAgentTask(ctx, current.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckClaim(ctx, task); err != nil {
		t.Fatalf("claim after duplicate publish: %v", err)
	}
	if !strings.Contains(task.HandoffNote.String, artifact) || !strings.Contains(task.HandoffNote.String, source) {
		t.Fatal("handoff lost source references")
	}

	// The topic inherits the issue's ACL; membership removal is checked on a
	// fresh history read and artifact access, without previews or raw URLs.
	outsider := dbfx.User(t, "Handoff outsider", "handoff-outsider-"+workflowTestSuffix()+"@multica.test")
	member := dbfx.Member(t, testWorkspaceID, outsider, "member")
	viewer := withURLParam(newRequest("GET", "/api/issues/"+issue+"/comments?thread_id="+root.ID, nil), "id", issue)
	viewer.Header.Set("X-User-ID", outsider)
	protected := middleware.RequireWorkspaceMember(testHandler.Queries)(http.HandlerFunc(testHandler.ListComments))
	before := httptest.NewRecorder()
	protected.ServeHTTP(before, viewer)
	if before.Code != http.StatusOK {
		t.Fatalf("initial member history: %d %s", before.Code, before.Body)
	}
	testHandler.MembershipCache.Set(ctx, outsider, testWorkspaceID)
	remove := withURLParams(newRequest("DELETE", "/api/workspaces/"+testWorkspaceID+"/members/"+member, nil), "id", testWorkspaceID, "memberId", member)
	testutil.Call(t, testHandler.DeleteMember, remove).Want(http.StatusNoContent)
	for _, user := range []string{testUserID, outsider} {
		r := withURLParam(newRequest("GET", "/api/issues/"+issue+"/comments?thread_id="+root.ID, nil), "id", issue)
		r.Header.Set("X-User-ID", user)
		h := middleware.RequireWorkspaceMember(testHandler.Queries)(http.HandlerFunc(testHandler.ListComments))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if user == testUserID && w.Code != 200 {
			t.Fatalf("authorized history: %d %s", w.Code, w.Body)
		}
		if user == outsider && (w.Code != 404 || strings.Contains(w.Body.String(), e.SourceRevision)) {
			t.Fatalf("source history leaked: %d %s", w.Code, w.Body)
		}
	}
	r := withURLParam(newRequest("GET", "/api/attachments/"+artifact+"/download", nil), "id", artifact)
	r.Header.Set("X-User-ID", outsider)
	testutil.Call(t, testHandler.DownloadAttachment, r).Want(http.StatusNotFound)

	if err := svc.Delete(ctx, parseUUID(issue), rule.ID, parseUUID(testUserID)); err != nil {
		t.Fatal(err)
	}
	r = withURLParams(newRequest("GET", "/api/issues/"+issue+"/wakeups/"+uuidToString(rule.ID)+"/runs", nil), "id", issue, "wakeupID", uuidToString(rule.ID))
	testutil.Call(t, testHandler.ListIssueWakeupRuns, r).Want(http.StatusNotFound)
	if err := svc.CheckClaim(ctx, task); err == nil {
		t.Fatal("removed subscription still claimable")
	}
}
