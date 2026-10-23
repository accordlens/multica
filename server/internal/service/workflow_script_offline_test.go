package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This transport exercises the real completion and sqlc call paths offline.
// It models rows and the completion CAS, not PostgreSQL locking or durability;
// the disposable-DB pilot remains the acceptance test for those properties.
type offlineWorkflowDB struct {
	t        *testing.T
	issue    db.Issue
	project  db.Project
	workflow db.IssueWorkflow
	agents   map[pgtype.UUID]db.Agent
	tasks    map[pgtype.UUID]db.AgentTaskQueue
	created  []db.AgentTaskQueue
	prHead   string
}

type offlineWorkflowRow struct {
	value any
	err   error
}

func (r offlineWorkflowRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	value := reflect.ValueOf(r.value)
	if value.Kind() != reflect.Struct {
		if len(dest) != 1 {
			return fmt.Errorf("scalar row has %d destinations", len(dest))
		}
		reflect.ValueOf(dest[0]).Elem().Set(value)
		return nil
	}
	if value.NumField() != len(dest) {
		return fmt.Errorf("%s row has %d fields, %d destinations", value.Type(), value.NumField(), len(dest))
	}
	for i, target := range dest {
		reflect.ValueOf(target).Elem().Set(value.Field(i))
	}
	return nil
}

func (f *offlineWorkflowDB) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	name := strings.Fields(query)[2]
	row := func(value any) pgx.Row { return offlineWorkflowRow{value: value} }
	none := offlineWorkflowRow{err: pgx.ErrNoRows}
	switch name {
	case "GetAgentTask":
		if task, ok := f.tasks[args[0].(pgtype.UUID)]; ok {
			return row(task)
		}
		return none
	case "GetAgent", "GetAgentInWorkspace":
		if agent, ok := f.agents[args[0].(pgtype.UUID)]; ok {
			return row(agent)
		}
		return none
	case "GetIssue", "GetIssueInWorkspace", "LockWorkflowIssue":
		return row(f.issue)
	case "GetProjectInWorkspace":
		return row(f.project)
	case "GetIssueWorkflow":
		return row(f.workflow)
	case "GetMemberByUserAndWorkspace":
		return row(db.Member{})
	case "GetIssueReviewHeadSha":
		if f.prHead != "" {
			return row(f.prHead)
		}
		return none
	case "LockChatSessionForTask", "RefreshAgentStatusFromTasks", "GetWorkspace", "GetIssueStatusEntryByKey":
		return none
	case "HasAgentCommentedSince":
		return row(true)
	case "HasSquadLeaderNoActionEvaluationForTask":
		return row(false)
	case "CompleteAgentTask":
		id := args[0].(pgtype.UUID)
		task := f.tasks[id]
		if task.Status != "running" {
			return none
		}
		task.Status, task.Result, task.WorkDir = "completed", args[1].([]byte), args[3].(pgtype.Text)
		f.tasks[id] = task
		return row(task)
	case "MoveWorkflowIssue":
		f.issue.Status = args[0].(string)
		f.issue.AssigneeID = args[1].(pgtype.UUID)
		return row(f.issue)
	case "CreateWorkflowSuccessor":
		source := f.tasks[args[6].(pgtype.UUID)]
		task := db.AgentTaskQueue{
			ID: args[0].(pgtype.UUID), AgentID: args[1].(pgtype.UUID), RuntimeID: args[2].(pgtype.UUID),
			Context: args[3].([]byte), WorkflowStep: pgtype.Text{String: args[4].(string), Valid: true},
			HandoffNote: pgtype.Text{String: args[5].(string), Valid: true}, ParentTaskID: source.ID,
			IssueID: source.IssueID, OriginatorUserID: source.OriginatorUserID, Status: "queued", MaxAttempts: 1,
		}
		f.tasks[task.ID] = task
		f.created = append(f.created, task)
		return row(task)
	case "SetWorkflowTaskContext":
		id := args[1].(pgtype.UUID)
		task := f.tasks[id]
		task.Context, _ = json.Marshal(map[string]json.RawMessage{"hybrid_workflow": args[0].([]byte)})
		f.tasks[id] = task
		return row(task)
	case "UpdateIssueStatus":
		f.issue.Status = args[1].(string)
		return row(f.issue)
	default:
		f.t.Fatalf("unexpected offline workflow query %s", name)
		return none
	}
}

func (f *offlineWorkflowDB) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Fields(query)[2] != "SettleTerminalTaskSupplements" {
		f.t.Fatalf("unexpected offline workflow exec %s", query)
	}
	return pgconn.NewCommandTag("UPDATE 0"), nil
}

func (f *offlineWorkflowDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	f.t.Fatal("unexpected offline workflow multi-row query")
	return nil, pgx.ErrNoRows
}

func offlineWorkflowID() pgtype.UUID { return pgtype.UUID{Bytes: uuid.New(), Valid: true} }

func newOfflineScriptPilot(t *testing.T) (scriptPilot, *offlineWorkflowDB, *TaskService, db.AgentTaskQueue) {
	t.Helper()
	owner, workspace, author, reviewer := offlineWorkflowID(), offlineWorkflowID(), offlineWorkflowID(), offlineWorkflowID()
	// Reuse the same real Git/Node fixture and frozen policy as the DB pilot.
	workdir := t.TempDir()
	cleanupPilotFiles(t, workdir)
	repo := workdir + "/pilot repo"
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	writePilotFile(t, repo+"/gate.mjs", pilotGate)
	writePilotFile(t, repo+"/answer.txt", "broken")
	pilotGit(t, repo, "init", "-q")
	pilotGit(t, repo, "add", ".")
	pilotGit(t, repo, "-c", "user.name=Pilot", "-c", "user.email=pilot@multica.test", "commit", "-qm", "intentional regression")
	script := issueworkflow.Script{ID: "pilot-unit", Version: "1", Checkout: "pilot repo", Directory: ".", Command: []string{"node", "gate.mjs"}, RequiredEnv: []string{"CI"}, TimeoutSeconds: 15, BudgetSeconds: 120, MaxRepairs: 1, RequiredChecks: []string{"unit"}, Files: map[string]string{"gate.mjs": issueworkflow.HashBytes([]byte(pilotGate))}}
	steps := issueworkflow.Arrange([]issueworkflow.Step{
		{StatusKey: "in_progress", Handler: issueworkflow.Handler{Type: "agent", ID: uuidTestString(author)}, NextStatusKey: "verify"},
		{StatusKey: "verify", Handler: issueworkflow.Handler{Type: "none"}, Script: &script, NextStatusKey: "in_review", BackStatusKey: "in_progress"},
		{StatusKey: "in_review", Handler: issueworkflow.Handler{Type: "agent", ID: uuidTestString(reviewer)}},
		{StatusKey: "blocked", Handler: issueworkflow.Handler{Type: "none"}},
	})
	raw, _ := issueworkflow.EncodeSteps(steps)
	workflow := db.IssueWorkflow{ID: offlineWorkflowID(), WorkspaceID: workspace, Steps: raw}
	def, err := issueworkflow.Decode(workflow)
	if err != nil {
		t.Fatal(err)
	}
	f := &offlineWorkflowDB{t: t, workflow: workflow, tasks: map[pgtype.UUID]db.AgentTaskQueue{}, agents: map[pgtype.UUID]db.Agent{}}
	f.project = db.Project{ID: offlineWorkflowID(), WorkspaceID: workspace, WorkflowID: workflow.ID}
	f.issue = db.Issue{ID: offlineWorkflowID(), WorkspaceID: workspace, ProjectID: f.project.ID, Status: "in_progress", AssigneeType: pgtype.Text{String: "agent", Valid: true}, AssigneeID: author}
	for _, id := range []pgtype.UUID{author, reviewer} {
		f.agents[id] = db.Agent{ID: id, WorkspaceID: workspace, OwnerID: owner, RuntimeID: offlineWorkflowID()}
	}
	task := db.AgentTaskQueue{ID: offlineWorkflowID(), IssueID: f.issue.ID, AgentID: author, RuntimeID: f.agents[author].RuntimeID, OriginatorUserID: owner, Status: "running", WorkflowStep: pgtype.Text{String: "in_progress", Valid: true}}
	f.tasks[task.ID] = task
	p := scriptPilot{author: uuidTestString(author), reviewer: uuidTestString(reviewer), def: def, workdir: workdir}
	return p, f, NewTaskService(db.New(f), nil, nil, events.New()), task
}

func completeOfflinePilot(t *testing.T, p scriptPilot, f *offlineWorkflowDB, svc *TaskService, task db.AgentTaskQueue, evidence issueworkflow.Evidence, output string) {
	t.Helper()
	task.Status = "running"
	f.tasks[task.ID] = task
	raw, _ := json.Marshal(workflowCompletion{Output: output, Evidence: &evidence})
	before := len(f.created)
	for attempt := range 3 {
		_, transitioned, err := svc.CompleteTaskWithTransition(context.Background(), task.ID, raw, "", p.workdir, "pilot", false, "", "")
		if err != nil || transitioned != (attempt == 0) {
			t.Fatalf("completion attempt %d transitioned=%v: %v", attempt, transitioned, err)
		}
		if attempt > 0 && len(f.created) > before+1 {
			t.Fatal("duplicate completion dispatched another workflow successor")
		}
	}
}

func writePilotFile(t *testing.T, filename, body string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestScriptWorkflowOfflineNativeFailRepairPassReviewHandoff(t *testing.T) {
	p, f, svc, task := newOfflineScriptPilot(t)
	completeOfflinePilot(t, p, f, svc, task, p.agentEvidence(t, task), "implemented")
	if len(f.created) != 1 {
		t.Fatal("author completion did not create exactly one script run")
	}
	script := f.created[0]
	failure := p.runScript(t, script)
	if failure.Outcome != "assertion_failed" {
		t.Fatalf("expected real script FAIL: %+v", failure)
	}
	completeOfflinePilot(t, p, f, svc, script, failure, "FAIL")
	repair := f.created[1]
	if execution := issueworkflow.ParseExecution(repair.Context); execution.Kind != "repair" || execution.Repairs != 1 || !strings.Contains(repair.HandoffNote.String, failure.ReportHash) {
		t.Fatal("repair lost its budget or real failure evidence")
	}
	writePilotFile(t, p.workdir+"/pilot repo/answer.txt", "42")
	pilotGit(t, p.workdir+"/pilot repo", "add", ".")
	pilotGit(t, p.workdir+"/pilot repo", "-c", "user.name=Pilot", "-c", "user.email=pilot@multica.test", "commit", "-qm", "repair")
	completeOfflinePilot(t, p, f, svc, repair, p.agentEvidence(t, repair), "fixed")
	retest := f.created[2]
	pass := p.runScript(t, retest)
	if pass.Outcome != "passed" || pass.Source.SHA == failure.Source.SHA {
		t.Fatalf("repair did not pass on a new real commit: %+v", pass)
	}
	if err := failure.Validate(uuidTestString(retest.ID), *issueworkflow.ParseExecution(retest.Context), time.Now()); err == nil {
		t.Fatal("earlier revision evidence satisfied the new gate")
	}
	completeOfflinePilot(t, p, f, svc, retest, pass, "PASS")
	review := f.created[3]
	if f.issue.Status != "in_review" || uuidTestString(review.AgentID) != p.reviewer || !strings.Contains(review.HandoffNote.String, pass.Report) {
		t.Fatal("independent handoff lost source evidence or workflow status")
	}
	completeOfflinePilot(t, p, f, svc, review, p.agentEvidence(t, review), "APPROVE "+pass.Source.SHA)
	if len(f.created) != 4 || f.issue.Status != "in_review" || issueworkflow.ParseExecution(f.tasks[review.ID].Context).ReviewVerdict != "APPROVE" {
		t.Fatal("duplicate handoff or automatic human acceptance")
	}
	t.Logf("native offline pilot: %s FAIL -> one repair -> %s PASS -> reviewer %s; four successors after fifteen callbacks", failure.Source.SHA, pass.Source.SHA, p.reviewer)
	receipt, _ := json.Marshal(map[string]any{"root_task_id": uuidTestString(task.ID), "workflow_id": uuidTestString(p.def.ID), "failure": failure, "pass": pass, "review_task_id": uuidTestString(review.ID), "successors": len(f.created), "callbacks": 15, "actors": "fake; no provider or independent QA invoked"})
	t.Logf("native_workflow_evidence=%s", receipt)
}

func TestScriptWorkflowOfflineNativeRejectsEvidenceAndBoundsRepair(t *testing.T) {
	for _, mode := range []string{"stale SHA", "empty", "skipped", "cancelled", "timeout", "repair limit", "no progress", "budget", "PR head"} {
		t.Run(mode, func(t *testing.T) {
			p, f, svc, author := newOfflineScriptPilot(t)
			completeOfflinePilot(t, p, f, svc, author, p.agentEvidence(t, author), "implemented")
			script := f.created[0]
			evidence := p.runScript(t, script)
			if evidence.Outcome != "assertion_failed" {
				t.Fatalf("fixture did not produce native failure evidence: %+v", evidence)
			}
			execution := issueworkflow.ParseExecution(script.Context)
			wantReason := ""
			switch mode {
			case "stale SHA", "empty", "skipped":
				var report issueworkflow.Report
				if err := json.Unmarshal([]byte(evidence.Report), &report); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "stale SHA":
					report.SourceSHA = strings.Repeat("0", 40)
					wantReason = "report does not match"
				case "empty":
					report.Checks = nil
					wantReason = "did not execute"
				case "skipped":
					report.Checks[0].Skipped = 1
					wantReason = "skipped tests"
				}
				raw, _ := json.Marshal(report)
				evidence.Report, evidence.ReportHash = string(raw), issueworkflow.HashBytes(raw)
			case "cancelled", "timeout":
				evidence.Outcome = mode
				wantReason = "script blocked: " + mode
			case "repair limit":
				execution.Repairs = execution.Script.MaxRepairs
				wantReason = "repair attempt limit exhausted"
			case "budget":
				execution.Deadline = time.Now().Add(-time.Second)
				wantReason = "workflow time budget exhausted"
			case "PR head":
				f.prHead = strings.Repeat("0", 40)
				wantReason = "source SHA differs from the linked PR head"
			}
			script.Context, _ = json.Marshal(map[string]any{"hybrid_workflow": execution})
			completeOfflinePilot(t, p, f, svc, script, evidence, "invalid gate")
			if mode == "no progress" {
				repair := f.created[1]
				completeOfflinePilot(t, p, f, svc, repair, p.agentEvidence(t, repair), "no revision change")
				script = repair
				wantReason = "repair made no revision progress"
			}
			wantSuccessors := 1
			if mode == "no progress" {
				wantSuccessors = 2
			}
			stored := issueworkflow.ParseExecution(f.tasks[script.ID].Context)
			if f.issue.Status != "blocked" || len(f.created) != wantSuccessors || !strings.Contains(stored.BlockReason, wantReason) {
				t.Fatalf("unsafe %s: status=%s successors=%d execution=%+v", mode, f.issue.Status, len(f.created), stored)
			}
		})
	}
}
