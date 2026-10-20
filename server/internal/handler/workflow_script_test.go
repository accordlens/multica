package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func scriptWorkflowFixture(t *testing.T) (IssueWorkflowResponse, string, string, string) {
	t.Helper()
	seedTestCatalog(t)
	author := dbfx.Agent(t, "workflow author", handlerTestRuntimeID(t))
	reviewer := dbfx.Agent(t, "workflow reviewer", handlerTestRuntimeID(t))
	verify := "verify_" + workflowTestSuffix()[6:]
	dbfx.Insert(t, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": verify, "name": "Verify", "category": "started", "color": "#123456"})
	script := issueworkflow.Script{ID: "code", Version: "1", Checkout: "repo", Directory: ".", Command: []string{"node", "gate.mjs"}, TimeoutSeconds: 5, BudgetSeconds: 120, MaxRepairs: 1, RequiredChecks: []string{"unit"}, Files: map[string]string{"gate.mjs": strings.Repeat("a", 64)}}
	wf := createTestWorkflowSteps(t, "Script "+workflowTestSuffix(), []map[string]any{
		{"status_key": "in_progress", "handler": map[string]string{"type": "agent", "id": author}, "next_status_key": verify},
		{"status_key": verify, "script": script, "next_status_key": "in_review", "back_status_key": "in_progress"},
		{"status_key": "in_review", "handler": map[string]string{"type": "agent", "id": reviewer}},
		{"status_key": "blocked"},
	})
	project := createWorkflowTestProject(t, "Script project")
	setProjectWorkflow(t, project, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issue := dbfx.Issue(t, "Script gate", testutil.Cols{"project_id": project, "status": "in_progress", "assignee_type": "agent", "assignee_id": author})
	return wf, project, issue, author
}

func TestScriptWorkflowStatusGateCannotBeBypassedWithAssignmentOrDone(t *testing.T) {
	_, _, issue, author := scriptWorkflowFixture(t)
	task := dbfx.Task(t, author, testutil.Cols{"issue_id": issue, "status": "running", "workflow_step": "in_progress", "runtime_id": handlerTestRuntimeID(t)})
	for _, target := range []string{"in_review", "done"} {
		agentUpdateIssue(t, issue, author, task, map[string]any{"status": target, "assignee_type": "agent", "assignee_id": author}).Want(http.StatusConflict)
		updateIssueForTest(t, issue, map[string]any{"status": target}).Want(http.StatusConflict)
	}
	if status, _, _ := issueStatusAndAssignee(t, issue); status != "in_progress" {
		t.Fatalf("gate bypassed: %s", status)
	}
	updateIssueForTest(t, issue, map[string]any{"status": "blocked"}).Want(http.StatusOK)
}

func TestScriptGateDefinitionAndBindingCannotBeChangedByRepairAgent(t *testing.T) {
	wf, project, issue, author := scriptWorkflowFixture(t)
	task := dbfx.Task(t, author, testutil.Cols{"issue_id": issue, "status": "running", "workflow_step": "in_progress", "runtime_id": handlerTestRuntimeID(t)})
	asRepair := func(r *http.Request) *http.Request {
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Agent-ID", author)
		r.Header.Set("X-Task-ID", task)
		return r
	}
	patch := withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+wf.ID, map[string]any{"steps": []map[string]any{{"status_key": "in_progress"}, {"status_key": "in_review"}}}), "id", wf.ID)
	testutil.Call(t, testHandler.UpdateIssueWorkflow, asRepair(patch)).Want(http.StatusForbidden)
	remove := withURLParam(workflowRequest("DELETE", "/api/issue-workflows/"+wf.ID, nil), "id", wf.ID)
	testutil.Call(t, testHandler.DeleteIssueWorkflow, asRepair(remove)).Want(http.StatusForbidden)
	switchWorkflow := withURLParam(workflowRequest("POST", "/api/projects/"+project+"/workflow", map[string]any{"workflow_id": nil}), "id", project)
	testutil.Call(t, testHandler.SetProjectWorkflow, asRepair(switchWorkflow)).Want(http.StatusForbidden)
	updateIssueForTest(t, issue, map[string]any{"project_id": nil}).Want(http.StatusConflict)
	stored, err := testHandler.Queries.GetIssueWorkflow(context.Background(), db.GetIssueWorkflowParams{ID: parseUUID(wf.ID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	def, err := issueworkflow.Decode(stored)
	if err != nil || !def.HasScripts() {
		t.Fatal("repair agent removed gate")
	}
}

func TestScriptEvidenceProjectionAndPostgresRoundTrip(t *testing.T) {
	evidence := &issueworkflow.Evidence{RunID: "run", Report: "{\n  \"checks\": []\n}", ReportHash: issueworkflow.HashBytes([]byte("{\n  \"checks\": []\n}")), Log: "log\x00tail"}
	req := TaskCompleteRequest{WorkflowEvidence: evidence}
	sanitizeTaskCompleteRequest(&req)
	if req.WorkflowEvidence.Log != "logtail" || req.WorkflowEvidence.Report != evidence.Report {
		t.Fatal("evidence sanitization corrupted report or left NUL")
	}
}

func TestScriptWorkflowClaimPinsDefinitionAndObserverCannotAcquireGate(t *testing.T) {
	_, _, issueID, author := scriptWorkflowFixture(t)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	observer := dbfx.Agent(t, "observer", handlerTestRuntimeID(t))
	task := db.AgentTaskQueue{AgentID: parseUUID(author)}
	if got := testHandler.claimProjectWorkflow(context.Background(), issue, &task); got == nil || got.Gate == nil || got.Gate.WorkflowVersion == "" {
		t.Fatal("assignee has no versioned gate")
	}
	task.AgentID = parseUUID(observer)
	if got := testHandler.claimProjectWorkflow(context.Background(), issue, &task); got == nil || got.Gate != nil {
		t.Fatal("observer acquired script authority")
	}
}

func TestScriptWorkflowForeignIssueAgentCannotSupplyHumanAcceptance(t *testing.T) {
	_, _, issue, author := scriptWorkflowFixture(t)
	other := dbfx.Issue(t, "Other task")
	task := dbfx.Task(t, author, testutil.Cols{"issue_id": other, "status": "running", "workflow_step": "in_progress", "runtime_id": handlerTestRuntimeID(t)})
	dbfx.Exec(t, "UPDATE issue SET status='in_review' WHERE id=$1", issue)
	response := agentUpdateIssue(t, issue, author, task, map[string]any{"status": "done"}).Want(http.StatusConflict)
	var body struct {
		Error string `json:"error"`
	}
	response.JSON(&body)
	if !strings.Contains(body.Error, "human acceptance") {
		t.Fatalf("foreign-issue agent was treated as a member: %s", body.Error)
	}
}
