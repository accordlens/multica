package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const pilotGate = `import {readFileSync,writeFileSync} from 'node:fs';
const p=process.env;const passed=readFileSync('answer.txt','utf8').trim()==='42';
const report={run_id:p.MULTICA_WORKFLOW_RUN_ID,source_sha:p.MULTICA_WORKFLOW_SOURCE_SHA,workflow_version:p.MULTICA_WORKFLOW_VERSION,script_version:p.MULTICA_WORKFLOW_SCRIPT_VERSION,checks:[{name:'unit',passed:passed?1:0,failed:passed?0:1,skipped:0}]};
writeFileSync(p.MULTICA_WORKFLOW_REPORT,JSON.stringify(report,null,2));console.log(passed?'answer regression PASS':'answer regression FAIL');process.exitCode=passed?0:1;`

type scriptPilot struct {
	f                principalFixture
	issue            pgtype.UUID
	author, reviewer string
	def              issueworkflow.Definition
	workdir          string
}

func pilotGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, raw, err)
	}
	return strings.TrimSpace(string(raw))
}

func newScriptPilot(t *testing.T) scriptPilot {
	t.Helper()
	f, owner := newPrincipalFixture(t)
	if err := f.q.SeedIssueStatusEntries(context.Background(), parseTestUUID(t, f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	author := f.privateAgentOwnedBy(t, owner, "script-author")
	reviewer := f.privateAgentOwnedBy(t, owner, "script-reviewer")
	f.Insert(t, "issue_status", testutil.Cols{"workspace_id": f.WorkspaceID, "key": "verify", "name": "Verify", "category": "started", "color": "#123456"})
	workdir := t.TempDir()
	repo := filepath.Join(workdir, "pilot repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repo, "gate.mjs"), []byte(pilotGate), 0600)
	_ = os.WriteFile(filepath.Join(repo, "answer.txt"), []byte("broken"), 0600)
	pilotGit(t, repo, "init", "-q")
	pilotGit(t, repo, "add", ".")
	pilotGit(t, repo, "-c", "user.name=Pilot", "-c", "user.email=pilot@multica.test", "commit", "-qm", "intentional regression")
	script := issueworkflow.Script{ID: "pilot-unit", Version: "1", Checkout: "pilot repo", Directory: ".", Command: []string{"node", "gate.mjs"}, RequiredEnv: []string{"CI"}, TimeoutSeconds: 5, BudgetSeconds: 120, MaxRepairs: 1, RequiredChecks: []string{"unit"}, Files: map[string]string{"gate.mjs": issueworkflow.HashBytes([]byte(pilotGate))}}
	steps := issueworkflow.Arrange([]issueworkflow.Step{{StatusKey: "in_progress", Handler: issueworkflow.Handler{Type: "agent", ID: author}, NextStatusKey: "verify"}, {StatusKey: "verify", Handler: issueworkflow.Handler{Type: "none"}, Script: &script, NextStatusKey: "in_review", BackStatusKey: "in_progress"}, {StatusKey: "in_review", Handler: issueworkflow.Handler{Type: "agent", ID: reviewer}}, {StatusKey: "blocked", Handler: issueworkflow.Handler{Type: "none"}}})
	raw, _ := issueworkflow.EncodeSteps(steps)
	row, err := f.q.CreateIssueWorkflow(context.Background(), db.CreateIssueWorkflowParams{WorkspaceID: parseTestUUID(t, f.WorkspaceID), Name: "Script pilot", InitialStatusKey: "todo", Steps: raw})
	if err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM issue_workflow WHERE id=$1", row.ID)
	project := f.Project(t, "Script pilot", testutil.Cols{"workflow_id": row.ID})
	issue := f.Issue(t, "Script pilot", testutil.Cols{"project_id": project, "status": "in_progress", "assignee_type": "agent", "assignee_id": author})
	f.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issue)
	f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", issue)
	def, err := issueworkflow.Decode(row)
	if err != nil {
		t.Fatal(err)
	}
	return scriptPilot{f: f, issue: parseTestUUID(t, issue), author: author, reviewer: reviewer, def: def, workdir: workdir}
}

func (p scriptPilot) task(t *testing.T) db.AgentTaskQueue {
	t.Helper()
	id := p.f.Task(t, p.author, testutil.Cols{"issue_id": p.issue, "originator_user_id": p.f.UserID, "accountable_user_id": p.f.UserID, "runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + p.author + "')"), "status": "running", "started_at": time.Now(), "workflow_step": "in_progress"})
	task, err := p.f.q.GetAgentTask(context.Background(), parseTestUUID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (p scriptPilot) agentEvidence(t *testing.T, task db.AgentTaskQueue) issueworkflow.Evidence {
	t.Helper()
	return issueworkflow.Evidence{RunID: uuidTestString(task.ID), Source: issueworkflow.Source{SHA: pilotGit(t, filepath.Join(p.workdir, "pilot repo"), "rev-parse", "HEAD")}, WorkflowVersion: p.def.Version(), Outcome: "agent_completed"}
}

func uuidTestString(id pgtype.UUID) string { raw, _ := id.Value(); return raw.(string) }

func (p scriptPilot) complete(t *testing.T, task db.AgentTaskQueue, evidence issueworkflow.Evidence, output string) {
	t.Helper()
	p.f.Exec(t, "UPDATE agent_task_queue SET status='running',started_at=now() WHERE id=$1 AND status='queued'", task.ID)
	raw, _ := json.Marshal(workflowCompletion{Output: output, Evidence: &evidence})
	if _, err := p.f.svc.TaskSvc.CompleteTask(context.Background(), task.ID, raw, "", p.workdir, "pilot", false, "", ""); err != nil {
		t.Fatal(err)
	}
}

func (p scriptPilot) successor(t *testing.T, source db.AgentTaskQueue, kind, status, agent string) db.AgentTaskQueue {
	t.Helper()
	tasks, err := p.f.q.ListActiveTasksByIssue(context.Background(), p.issue)
	if err != nil {
		t.Fatal(err)
	}
	var found []db.AgentTaskQueue
	for _, task := range tasks {
		if task.ParentTaskID == source.ID {
			found = append(found, task)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one durable %s successor, got %d (active %d)", kind, len(found), len(tasks))
	}
	got := found[0]
	execution := issueworkflow.ParseExecution(got.Context)
	if execution == nil || execution.Kind != kind || got.WorkflowStep.String != status || uuidTestString(got.AgentID) != agent || got.MaxAttempts != 1 {
		t.Fatalf("wrong successor: %+v / %+v", got, execution)
	}
	issue, err := p.f.q.GetIssue(context.Background(), p.issue)
	if err != nil || issue.Status != status || issue.AssigneeID != got.AgentID {
		t.Fatalf("issue and run disagree: %s %v", issue.Status, err)
	}
	return got
}

func (p scriptPilot) runScript(t *testing.T, task db.AgentTaskQueue) issueworkflow.Evidence {
	t.Helper()
	execution := issueworkflow.ParseExecution(task.Context)
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	request := map[string]any{"run_id": uuidTestString(task.ID), "execution": execution}
	written := make(chan error, 1)
	go func() { written <- json.NewEncoder(w).Encode(request) }()
	var output bytes.Buffer
	if err := daemon.RunWorkflowScriptHelper(r, &output); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	var response struct {
		Evidence issueworkflow.Evidence `json:"evidence"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Evidence
}

func TestScriptWorkflowPilotFailRepairPassIndependentReview(t *testing.T) {
	p := newScriptPilot(t)
	author := p.task(t)
	p.complete(t, author, p.agentEvidence(t, author), "implemented")
	script := p.successor(t, author, "script", "verify", p.author)
	failure := p.runScript(t, script)
	if failure.Outcome != "assertion_failed" {
		t.Fatalf("intentional failure did not stop gate: %+v", failure)
	}
	p.complete(t, script, failure, "unit FAIL")
	repair := p.successor(t, script, "repair", "in_progress", p.author)
	if !strings.Contains(repair.HandoffNote.String, failure.ReportHash) || !strings.Contains(repair.HandoffNote.String, "answer regression FAIL") {
		t.Fatal("repair did not receive the report and log")
	}
	// The fake author fixes code, not the pinned gate; no model is invoked.
	repo := filepath.Join(p.workdir, "pilot repo")
	_ = os.WriteFile(filepath.Join(repo, "answer.txt"), []byte("42"), 0600)
	pilotGit(t, repo, "add", ".")
	pilotGit(t, repo, "-c", "user.name=Pilot", "-c", "user.email=pilot@multica.test", "commit", "-qm", "fix answer")
	p.complete(t, repair, p.agentEvidence(t, repair), "fixed")
	retest := p.successor(t, repair, "script", "verify", p.author)
	pass := p.runScript(t, retest)
	if pass.Outcome != "passed" || pass.Source.SHA == failure.Source.SHA {
		t.Fatalf("new revision did not pass: %+v", pass)
	}
	p.complete(t, retest, pass, "unit PASS")
	review := p.successor(t, retest, "review", "in_review", p.reviewer)
	if !strings.Contains(review.HandoffNote.String, pass.ReportHash) || !strings.Contains(review.HandoffNote.String, pass.Report) {
		t.Fatal("reviewer did not receive real report")
	}
	p.complete(t, review, p.agentEvidence(t, review), "Independent review\nAPPROVE "+pass.Source.SHA)
	stored, err := p.f.q.GetAgentTask(context.Background(), review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if execution := issueworkflow.ParseExecution(stored.Context); execution.ReviewVerdict != "APPROVE" || execution.AuthorID == p.reviewer {
		t.Fatalf("independent verdict not retained: %+v", execution)
	}
	issue, _ := p.f.q.GetIssue(context.Background(), p.issue)
	if issue.Status != "in_review" {
		t.Fatal("human acceptance was skipped")
	}
}

func TestScriptWorkflowDuplicateCompletionCreatesOneSuccessor(t *testing.T) {
	p := newScriptPilot(t)
	author := p.task(t)
	evidence := p.agentEvidence(t, author)
	raw, _ := json.Marshal(workflowCompletion{Output: "implemented", Evidence: &evidence})
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.f.svc.TaskSvc.CompleteTask(context.Background(), author.ID, raw, "", p.workdir, "pilot", false, "", "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p.successor(t, author, "script", "verify", p.author)
}

func TestScriptWorkflowRejectsNoProgressAndRepairLimit(t *testing.T) {
	for _, mode := range []string{"no progress", "limit"} {
		t.Run(mode, func(t *testing.T) {
			p := newScriptPilot(t)
			author := p.task(t)
			p.complete(t, author, p.agentEvidence(t, author), "implemented")
			script := p.successor(t, author, "script", "verify", p.author)
			p.complete(t, script, p.runScript(t, script), "FAIL")
			repair := p.successor(t, script, "repair", "in_progress", p.author)
			if mode == "limit" {
				repo := filepath.Join(p.workdir, "pilot repo")
				_ = os.WriteFile(filepath.Join(repo, "answer.txt"), []byte("still broken"), 0600)
				pilotGit(t, repo, "add", ".")
				pilotGit(t, repo, "-c", "user.name=P", "-c", "user.email=p@multica.test", "commit", "-qm", "unsuccessful repair")
			}
			p.complete(t, repair, p.agentEvidence(t, repair), "repair")
			if mode == "limit" {
				retest := p.successor(t, repair, "script", "verify", p.author)
				p.complete(t, retest, p.runScript(t, retest), "FAIL")
			}
			issue, _ := p.f.q.GetIssue(context.Background(), p.issue)
			active, _ := p.f.q.ListActiveTasksByIssue(context.Background(), p.issue)
			if issue.Status != "blocked" || len(active) != 0 {
				t.Fatalf("unbounded repair: status=%s active=%d", issue.Status, len(active))
			}
		})
	}
}

func TestScriptWorkflowOfflineCancelAndStaleReportBlockWithoutReplay(t *testing.T) {
	for _, mode := range []string{"offline", "cancel", "stale report", "definition changed"} {
		t.Run(mode, func(t *testing.T) {
			p := newScriptPilot(t)
			author := p.task(t)
			p.complete(t, author, p.agentEvidence(t, author), "implemented")
			script := p.successor(t, author, "script", "verify", p.author)
			var err error
			switch mode {
			case "offline":
				p.f.Exec(t, "UPDATE agent_task_queue SET status='running',started_at=now() WHERE id=$1", script.ID)
				_, err = p.f.svc.TaskSvc.FailTask(context.Background(), script.ID, "worker offline", "", p.workdir, "", "runtime_offline", false, "", "")
			case "cancel":
				_, err = p.f.svc.TaskSvc.CancelTask(context.Background(), script.ID)
			default:
				evidence := p.runScript(t, script)
				if mode == "stale report" {
					evidence.RunID = uuidTestString(author.ID)
				} else {
					p.f.Exec(t, "UPDATE issue_workflow SET steps=jsonb_set(steps,'{2,script,version}','\"2\"'::jsonb) WHERE id=$1", p.def.ID)
				}
				p.complete(t, script, evidence, "invalid proof")
			}
			if err != nil {
				t.Fatal(err)
			}
			issue, _ := p.f.q.GetIssue(context.Background(), p.issue)
			active, _ := p.f.q.ListActiveTasksByIssue(context.Background(), p.issue)
			if issue.Status != "blocked" || len(active) != 0 {
				t.Fatalf("unsafe terminal handling: status=%s active=%d", issue.Status, len(active))
			}
		})
	}
}

func TestScriptWorkflowBudgetExpiresQueuedWorkOnExistingSweep(t *testing.T) {
	p := newScriptPilot(t)
	author := p.task(t)
	p.complete(t, author, p.agentEvidence(t, author), "implemented")
	script := p.successor(t, author, "script", "verify", p.author)
	p.f.Exec(t, "UPDATE agent_task_queue SET context=jsonb_set(context,'{hybrid_workflow,deadline}',to_jsonb($2::text)) WHERE id=$1", script.ID, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano))
	expired, err := p.f.svc.TaskSvc.ExpireStaleQueuedTasks(context.Background(), db.ExpireStaleQueuedTasksParams{ReconnectGraceSecs: 999999, MaxPerTick: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != script.ID || expired[0].FailureReason.String != "workflow_budget_exhausted" {
		t.Fatalf("budget not enforced: %+v", expired)
	}
	if retried := p.f.svc.TaskSvc.HandleFailedTasks(context.Background(), expired); retried != 0 {
		t.Fatal("expired script replayed")
	}
	issue, _ := p.f.q.GetIssue(context.Background(), p.issue)
	if issue.Status != "blocked" {
		t.Fatalf("expiry not visible: %s", issue.Status)
	}
}
