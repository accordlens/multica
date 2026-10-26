package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func pilotDependency(t *testing.T, p scriptPilot, status string) string {
	t.Helper()
	source := p.f.Issue(t, "Required input", testutil.Cols{"status": status})
	p.f.Insert(t, "issue_dependency", testutil.Cols{
		"issue_id": p.issue, "depends_on_issue_id": source, "type": "blocked_by",
		"condition": testutil.Raw(`'{"kind":"done"}'::jsonb`),
	})
	return source
}

func claimPilotTask(t *testing.T, p scriptPilot, task db.AgentTaskQueue) db.AgentTaskQueue {
	t.Helper()
	claimed, err := p.f.svc.TaskSvc.ClaimTask(context.Background(), task.AgentID)
	if err != nil || claimed == nil || claimed.ID != task.ID {
		t.Fatalf("claim expected workflow run: got=%v err=%v", claimed, err)
	}
	started, err := p.f.svc.TaskSvc.StartTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return *started
}

func completePilotConcurrently(t *testing.T, p scriptPilot, task db.AgentTaskQueue, evidence issueworkflow.Evidence, output string) {
	t.Helper()
	raw, err := json.Marshal(workflowCompletion{Output: output, Evidence: &evidence})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		transitioned bool
		err          error
	}
	results := make(chan result, 6)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, transitioned, err := p.f.svc.TaskSvc.CompleteTaskWithTransition(context.Background(), task.ID, raw, "", p.workdir, "pilot", false, "", "")
			results <- result{transitioned, err}
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.transitioned {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("completion CAS winners=%d", winners)
	}
}

// The same scenario as the offline transport test, using PostgreSQL admission,
// row locks, completion transactions and native queue rows. Actors are fixtures;
// this exercises independent routing without invoking a model or accepting QA.
func TestScriptWorkflowDatabaseDependenciesAndRevisionEvidence(t *testing.T) {
	p := newScriptPilot(t)
	a, b := pilotDependency(t, p, "in_progress"), pilotDependency(t, p, "in_progress")
	root := p.task(t)
	p.f.Exec(t, "UPDATE agent_task_queue SET status='queued',started_at=NULL WHERE id=$1", root.ID)
	for _, statuses := range [][2]string{{"in_progress", "in_progress"}, {"in_progress", "done"}, {"done", "in_progress"}, {"done", "in_review"}, {"done", "cancelled"}} {
		p.f.Exec(t, "UPDATE issue SET status=$2 WHERE id=$1", a, statuses[0])
		p.f.Exec(t, "UPDATE issue SET status=$2 WHERE id=$1", b, statuses[1])
		claimed, err := p.f.svc.TaskSvc.ClaimTask(context.Background(), root.AgentID)
		if err != nil || claimed != nil {
			t.Fatalf("unmet AND gate claimed work: A=%s B=%s claimed=%v err=%v", statuses[0], statuses[1], claimed, err)
		}
	}
	p.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", b)
	claimed, err := p.f.svc.TaskSvc.ClaimTask(context.Background(), root.AgentID)
	if err != nil || claimed == nil || claimed.ID != root.ID {
		t.Fatalf("all inputs ready did not claim original run: %v %v", claimed, err)
	}
	p.f.Exec(t, "UPDATE issue SET status='in_progress' WHERE id=$1", b)
	if _, err := p.f.svc.TaskSvc.StartTask(context.Background(), root.ID); err == nil {
		t.Fatal("a delivered claim started after its prerequisite reopened")
	}
	p.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", b)
	if _, err := p.f.svc.TaskSvc.StartTask(context.Background(), root.ID); err != nil {
		t.Fatal(err)
	}
	completePilotConcurrently(t, p, root, p.agentEvidence(t, root), "implemented")
	script := claimPilotTask(t, p, p.successor(t, root, "script", "verify", p.author))
	failure := p.runScript(t, script)
	if failure.Outcome != "assertion_failed" {
		t.Fatalf("broken revision did not stop review: %+v", failure)
	}
	completePilotConcurrently(t, p, script, failure, "FAIL")
	repair := claimPilotTask(t, p, p.successor(t, script, "repair", "in_progress", p.author))
	if !strings.Contains(repair.HandoffNote.String, failure.ReportHash) {
		t.Fatal("repair lost the failing report")
	}
	repo := filepath.Join(p.workdir, "pilot repo")
	writePilotFile(t, filepath.Join(repo, "answer.txt"), "42")
	pilotGit(t, repo, "add", ".")
	pilotGit(t, repo, "-c", "user.name=Pilot", "-c", "user.email=pilot@multica.test", "commit", "-qm", "repair answer")
	completePilotConcurrently(t, p, repair, p.agentEvidence(t, repair), "fixed")
	retest := claimPilotTask(t, p, p.successor(t, repair, "script", "verify", p.author))
	pass := p.runScript(t, retest)
	if pass.Outcome != "passed" || pass.Source.SHA == failure.Source.SHA || pass.ReportHash == failure.ReportHash {
		t.Fatalf("repair reused stale source/report: failure=%+v pass=%+v", failure, pass)
	}
	completePilotConcurrently(t, p, retest, pass, "PASS")
	review := claimPilotTask(t, p, p.successor(t, retest, "review", "in_review", p.reviewer))
	if review.AgentID == root.AgentID || !strings.Contains(review.HandoffNote.String, pass.ReportHash) || !strings.Contains(review.HandoffNote.String, pass.Report) {
		t.Fatal("review lost independent identity or the current report")
	}
	completePilotConcurrently(t, p, review, p.agentEvidence(t, review), "APPROVE "+pass.Source.SHA)
	if count := p.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", p.issue); count != 5 {
		t.Fatalf("30 completion callbacks created %d runs; expected root + four successors", count)
	}
	issue, err := p.f.q.GetIssue(context.Background(), p.issue)
	if err != nil || issue.Status != "in_review" {
		t.Fatalf("workflow skipped human acceptance: status=%s err=%v", issue.Status, err)
	}
	receipt, _ := json.Marshal(map[string]any{
		"issue_id": uuidTestString(p.issue), "workflow_id": uuidTestString(p.def.ID), "workflow_version": p.def.Version(),
		"prerequisite_issue_ids": []string{a, b}, "root_run_id": uuidTestString(root.ID),
		"failure": failure, "pass": pass, "review_run_id": uuidTestString(review.ID), "reviewer_id": p.reviewer,
		"completion_callbacks": 30, "runs": 5, "database": "PostgreSQL", "actors": "fixture; no model or independent QA acceptance",
	})
	t.Logf("database_workflow_evidence=%s", receipt)
}

func TestScriptWorkflowDatabaseReopenPreservesCompletion(t *testing.T) {
	for _, status := range []string{"in_progress", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			p := newScriptPilot(t)
			source := pilotDependency(t, p, "done")
			repo := filepath.Join(p.workdir, "pilot repo")
			writePilotFile(t, filepath.Join(repo, "answer.txt"), "42")
			pilotGit(t, repo, "add", ".")
			pilotGit(t, repo, "-c", "user.name=Pilot", "-c", "user.email=pilot@multica.test", "commit", "-qm", "passing answer")
			root := p.task(t)
			p.complete(t, root, p.agentEvidence(t, root), "implemented")
			script := claimPilotTask(t, p, p.successor(t, root, "script", "verify", p.author))
			pass := p.runScript(t, script)
			if pass.Outcome != "passed" {
				t.Fatalf("fixture did not produce PASS: %+v", pass)
			}
			p.f.Exec(t, "UPDATE issue SET status=$2 WHERE id=$1", source, status)
			raw, _ := json.Marshal(workflowCompletion{Output: "PASS", Evidence: &pass})
			completed, transitioned, err := p.f.svc.TaskSvc.CompleteTaskWithTransition(context.Background(), script.ID, raw, "", p.workdir, "pilot", false, "", "")
			if err != nil || !transitioned || completed == nil || completed.Status != "completed" {
				t.Fatalf("declined handoff lost the completion: transitioned=%v completed=%v err=%v", transitioned, completed, err)
			}
			p.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", source)
			for range 2 {
				_, replayed, err := p.f.svc.TaskSvc.CompleteTaskWithTransition(context.Background(), script.ID, raw, "", p.workdir, "pilot", false, "", "")
				if err != nil || replayed {
					t.Fatalf("completion replay advanced a declined handoff: replayed=%v err=%v", replayed, err)
				}
			}
			stored, err := p.f.q.GetAgentTask(context.Background(), script.ID)
			if err != nil || stored.Status != "completed" {
				t.Fatalf("completion was not committed: %s %v", stored.Status, err)
			}
			var result workflowCompletion
			if err := json.Unmarshal(stored.Result, &result); err != nil || result.Output != "PASS" || !reflect.DeepEqual(result.Evidence, &pass) {
				t.Fatalf("durable evidence changed: %+v err=%v", result, err)
			}
			issue, err := p.f.q.GetIssue(context.Background(), p.issue)
			if err != nil || issue.Status != "verify" || p.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", p.issue) != 2 {
				t.Fatalf("unmet dependency advanced review: status=%s err=%v", issue.Status, err)
			}
		})
	}
}

func TestScriptWorkflowDatabaseRejectsStaleEvidence(t *testing.T) {
	for _, mode := range []string{"source SHA", "report SHA", "skipped", "empty report", "workflow version"} {
		t.Run(mode, func(t *testing.T) {
			p := newScriptPilot(t)
			repo := filepath.Join(p.workdir, "pilot repo")
			writePilotFile(t, filepath.Join(repo, "answer.txt"), "42")
			pilotGit(t, repo, "add", ".")
			pilotGit(t, repo, "-c", "user.name=Pilot", "-c", "user.email=pilot@multica.test", "commit", "-qm", "passing answer")
			root := p.task(t)
			p.complete(t, root, p.agentEvidence(t, root), "implemented")
			script := claimPilotTask(t, p, p.successor(t, root, "script", "verify", p.author))
			evidence := p.runScript(t, script)
			if evidence.Outcome != "passed" {
				t.Fatalf("fixture did not execute passing checks: %+v", evidence)
			}
			var report issueworkflow.Report
			if err := json.Unmarshal([]byte(evidence.Report), &report); err != nil {
				t.Fatal(err)
			}
			want := ""
			switch mode {
			case "source SHA":
				evidence.Source.SHA = strings.Repeat("0", 40)
				want = "evidence identity or revision mismatch"
			case "report SHA":
				report.SourceSHA = strings.Repeat("0", 40)
				want = "report does not match"
			case "skipped":
				report.Checks[0].Skipped = 1
				want = "skipped tests"
			case "empty report":
				report.Checks = nil
				want = "did not execute"
			case "workflow version":
				evidence.WorkflowVersion = strings.Repeat("0", 64)
				want = "missing current clean source attestation"
			}
			updated, _ := json.Marshal(report)
			evidence.Report = string(updated)
			evidence.ReportHash = issueworkflow.HashBytes(updated)
			p.complete(t, script, evidence, "claimed PASS")
			stored, err := p.f.q.GetAgentTask(context.Background(), script.ID)
			if err != nil || stored.Status != "completed" {
				t.Fatalf("invalid evidence did not retain the terminal result: %s %v", stored.Status, err)
			}
			execution := issueworkflow.ParseExecution(stored.Context)
			if execution == nil || !strings.Contains(execution.BlockReason, want) {
				t.Fatalf("wrong visible rejection: execution=%+v expected=%s", execution, want)
			}
			issue, err := p.f.q.GetIssue(context.Background(), p.issue)
			if err != nil || issue.Status != "blocked" || p.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", p.issue) != 2 {
				t.Fatalf("invalid evidence admitted review: status=%s err=%v", issue.Status, err)
			}
		})
	}
}

type workflowFailingBeginner struct{ TxStarter }
type workflowFailingTx struct{ pgx.Tx }
type workflowFailedRow struct{}

var errWorkflowSuccessorWrite = errors.New("injected workflow successor write failure")

func (b workflowFailingBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return workflowFailingTx{tx}, nil
}

func (tx workflowFailingTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "-- name: CreateWorkflowSuccessor") {
		return workflowFailedRow{}
	}
	return tx.Tx.QueryRow(ctx, query, args...)
}

func (workflowFailedRow) Scan(...any) error { return errWorkflowSuccessorWrite }

func TestScriptWorkflowDatabaseSuccessorFailureRollsBack(t *testing.T) {
	p := newScriptPilot(t)
	root := p.task(t)
	evidence := p.agentEvidence(t, root)
	raw, _ := json.Marshal(workflowCompletion{Output: "implemented", Evidence: &evidence})
	svc := p.f.svc.TaskSvc
	original := svc.TxStarter
	svc.TxStarter = workflowFailingBeginner{original}
	_, transitioned, err := svc.CompleteTaskWithTransition(context.Background(), root.ID, raw, "", p.workdir, "pilot", false, "", "")
	svc.TxStarter = original
	if !errors.Is(err, errWorkflowSuccessorWrite) || transitioned {
		t.Fatalf("successor write failure was swallowed: transitioned=%v err=%v", transitioned, err)
	}
	stored, err := p.f.q.GetAgentTask(context.Background(), root.ID)
	if err != nil || stored.Status != "running" || len(stored.Result) != 0 {
		t.Fatalf("failed transaction committed its completion: task=%+v err=%v", stored, err)
	}
	issue, err := p.f.q.GetIssue(context.Background(), p.issue)
	if err != nil || issue.Status != "in_progress" || p.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", p.issue) != 1 {
		t.Fatalf("failed transaction committed an issue move or successor: status=%s err=%v", issue.Status, err)
	}
	completePilotConcurrently(t, p, root, evidence, "implemented")
	p.successor(t, root, "script", "verify", p.author)
	if p.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", p.issue) != 2 {
		t.Fatal("retry after rollback duplicated work")
	}
}
