package service

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This joint scenario runs the native sqlc/service/daemon boundaries and real
// Git/Node script evidence. Readiness rows are explicit fixture projections,
// not an execution of PostgreSQL's AND predicates or a claim of DB/ACL PASS.
func TestScriptWorkflowOfflineJointDependenciesAndRevisionEvidence(t *testing.T) {
	p, f, svc, root := newOfflineScriptPilot(t)
	a, b := offlineWorkflowID(), offlineWorkflowID()
	f.dependencyProjection = map[pgtype.UUID]bool{a: false, b: false}
	root.Status = "dispatched"
	f.tasks[root.ID] = root
	for _, readiness := range [][2]bool{{false, false}, {true, false}, {false, true}} {
		f.dependencyProjection[a], f.dependencyProjection[b] = readiness[0], readiness[1]
		if _, err := svc.StartTask(context.Background(), root.ID); err == nil {
			t.Fatalf("started C with readiness A=%v B=%v", readiness[0], readiness[1])
		}
		if f.tasks[root.ID].Status != "dispatched" || len(f.started) != 0 || len(f.created) != 0 {
			t.Fatal("unmet dependency changed the native run or created work")
		}
	}
	f.dependencyProjection[a], f.dependencyProjection[b] = true, true
	start := func(task db.AgentTaskQueue) db.AgentTaskQueue {
		t.Helper()
		task.Status = "dispatched" // Claim transport is outside this offline test.
		f.tasks[task.ID] = task
		started, err := svc.StartTask(context.Background(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return *started
	}
	root = start(root)
	completeOfflinePilot(t, p, f, svc, root, p.agentEvidence(t, root), "implemented")
	if len(f.created) != 1 {
		t.Fatal("one author completion must create one script run")
	}
	script := start(f.created[0])
	failure := p.runScript(t, script)
	if failure.Outcome != "assertion_failed" {
		t.Fatalf("real fixture script did not FAIL: %+v", failure)
	}
	completeOfflinePilot(t, p, f, svc, script, failure, "FAIL")
	repair := start(f.created[1])
	if execution := issueworkflow.ParseExecution(repair.Context); execution.Kind != "repair" || execution.Repairs != 1 {
		t.Fatal("FAIL did not route exactly one bounded repair")
	}
	writePilotFile(t, p.workdir+"/pilot repo/answer.txt", "42")
	pilotGit(t, p.workdir+"/pilot repo", "add", ".")
	pilotGit(t, p.workdir+"/pilot repo", "-c", "user.name=Pilot", "-c", "user.email=pilot@multica.test", "commit", "-qm", "joint dependency repair")
	completeOfflinePilot(t, p, f, svc, repair, p.agentEvidence(t, repair), "fixed")
	retest := start(f.created[2])
	pass := p.runScript(t, retest)
	if pass.Outcome != "passed" || pass.Source.SHA == failure.Source.SHA || pass.ReportHash == failure.ReportHash {
		t.Fatalf("repair did not produce PASS for a new revision/report: %+v", pass)
	}
	for _, mode := range []string{"reopened prerequisite", "stale SHA", "stale run", "stale report"} {
		t.Run(mode, func(t *testing.T) {
			copy := *f
			copy.t, copy.tasks, copy.created = t, maps.Clone(f.tasks), slices.Clone(f.created)
			copy.dependencyProjection = maps.Clone(f.dependencyProjection)
			transport := NewTaskService(db.New(&copy), nil, nil, events.New())
			bad := pass
			switch mode {
			case "reopened prerequisite":
				copy.dependencyProjection[b] = false
			case "stale SHA":
				bad.Source.SHA = failure.Source.SHA
			case "stale run":
				bad.RunID = failure.RunID
			case "stale report":
				bad.Report, bad.ReportHash = failure.Report, failure.ReportHash
			}
			raw, _ := json.Marshal(workflowCompletion{Output: "PASS", Evidence: &bad})
			completed, transitioned, err := transport.CompleteTaskWithTransition(context.Background(), retest.ID, raw, "", p.workdir, "joint", false, "", "")
			if mode == "reopened prerequisite" {
				if err != nil || !transitioned || completed == nil || completed.Status != "completed" || copy.tasks[retest.ID].Status != "completed" {
					t.Fatalf("unmet dependency must save the completion: transitioned=%v task=%+v err=%v", transitioned, completed, err)
				}
				if copy.issue.Status != "verify" || !bytes.Equal(completed.Result, raw) || !bytes.Equal(copy.tasks[retest.ID].Result, raw) {
					t.Fatal("unmet dependency moved the issue or lost the PASS evidence")
				}
				// Readiness returning cannot replay an already recorded completion
				// into another handoff. PostgreSQL rollback remains a DB gate.
				copy.dependencyProjection[b] = true
				for range 2 {
					_, replayed, replayErr := transport.CompleteTaskWithTransition(context.Background(), retest.ID, raw, "", p.workdir, "joint", false, "", "")
					if replayErr != nil || replayed || copy.issue.Status != "verify" || !bytes.Equal(copy.tasks[retest.ID].Result, raw) {
						t.Fatalf("replay changed the recorded completion: transitioned=%v err=%v", replayed, replayErr)
					}
				}
			} else if err != nil || copy.issue.Status != "blocked" {
				t.Fatalf("invalid %s evidence was not blocked: status=%s err=%v", mode, copy.issue.Status, err)
			}
			if len(copy.created) != 3 {
				t.Fatal("rejected dependency/evidence created a review successor")
			}
		})
	}
	completeOfflinePilot(t, p, f, svc, retest, pass, "PASS")
	if len(f.created) != 4 {
		t.Fatal("PASS replay created duplicate review work")
	}
	review := f.created[3]
	if review.AgentID == root.AgentID || uuidTestString(review.AgentID) != p.reviewer || !strings.Contains(review.HandoffNote.String, pass.ReportHash) {
		t.Fatal("review routing lost independent identity or current report provenance")
	}
	f.dependencyProjection[b] = false
	review.Status = "dispatched"
	f.tasks[review.ID] = review
	if _, err := svc.StartTask(context.Background(), review.ID); err == nil {
		t.Fatal("reopened input admitted review")
	}
	f.dependencyProjection[b] = true
	review = start(review)
	completeOfflinePilot(t, p, f, svc, review, p.agentEvidence(t, review), "APPROVE "+pass.Source.SHA)
	if len(f.created) != 4 || len(f.started) != 5 || f.issue.Status != "in_review" {
		t.Fatal("replay duplicated logical work or bypassed human acceptance")
	}
	receipt, _ := json.Marshal(map[string]any{
		"issue_id": uuidTestString(f.issue.ID), "workflow_id": uuidTestString(p.def.ID), "workflow_version": p.def.Version(),
		"root_task_id": uuidTestString(root.ID), "prerequisite_issue_ids": []string{uuidTestString(a), uuidTestString(b)},
		"failure": failure, "pass": pass, "review_task_id": uuidTestString(review.ID), "reviewer_id": p.reviewer,
		"successors": len(f.created), "started_runs": len(f.started), "completion_callbacks": 15,
		"readiness_boundary": "fixture projections; PostgreSQL AND/locking/rollback/ACL NOT_RUN", "actors": "fake; no independent QA acceptance",
	})
	t.Logf("joint_native_workflow_evidence=%s", receipt)
}
