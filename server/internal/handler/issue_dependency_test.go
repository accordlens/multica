package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func relationParam(r *http.Request, key, value string) *http.Request {
	chi.RouteContext(r.Context()).URLParams.Add(key, value)
	return r
}

func relationRequest(method, issue, suffix string, body any) *http.Request {
	r := withURLParam(newRequest(method, "/api/issues/"+issue+"/relations"+suffix, body), "id", issue)
	return r
}

func addDependency(t *testing.T, dependent, source, kind string) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	testutil.Call(t, testHandler.CreateIssueDependency, relationRequest("POST", dependent, "", map[string]any{
		"issue_id": source, "condition": map[string]any{"kind": kind},
	})).Want(201).JSON(&out)
	return out.ID
}

func TestIssueDependencyConcurrentCycleAndReplay(t *testing.T) {
	a, b := dbfx.Issue(t, "A"), dbfx.Issue(t, "B")
	start := make(chan struct{})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		wg.Add(1)
		go func(pair [2]string) {
			defer wg.Done()
			<-start
			w := httptest.NewRecorder()
			testHandler.CreateIssueDependency(w, relationRequest("POST", pair[0], "", map[string]any{"issue_id": pair[1]}))
			codes <- w.Code
		}(pair)
	}
	close(start)
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatalf("concurrent cycle statuses: %v", counts)
	}
	var dependent, source string
	dbfx.QueryRow(t, `SELECT issue_id::text, depends_on_issue_id::text FROM issue_dependency WHERE issue_id IN ($1,$2)`, a, b).Scan(&dependent, &source)
	for range 3 {
		testutil.Call(t, testHandler.CreateIssueDependency, relationRequest("POST", dependent, "", map[string]any{"issue_id": source})).Want(200)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_dependency WHERE issue_id IN ($1,$2)`, a, b); n != 1 {
		t.Fatalf("replay wrote %d relations", n)
	}
	testutil.Call(t, testHandler.CreateIssueDependency, relationRequest("POST", a, "", map[string]any{"issue_id": a})).Want(400)
	// Context links can form a network and do not create an execution cycle.
	testutil.Call(t, testHandler.CreateIssueDependency, relationRequest("POST", source, "", map[string]any{"issue_id": dependent, "type": "related"})).Want(201)
}

func TestIssueDependencyANDGateClaimsOnceAndReopens(t *testing.T) {
	ctx := context.Background()
	a, b, c := dbfx.Issue(t, "A"), dbfx.Issue(t, "B"), dbfx.Issue(t, "C")
	addDependency(t, c, a, "done")
	addDependency(t, c, b, "done")
	runtime := dbfx.Runtime(t, "dependency-runtime")
	agent := dbfx.Agent(t, "dependency-agent", runtime, testutil.Cols{"max_concurrent_tasks": 8})
	run := dbfx.Task(t, agent, testutil.Cols{"issue_id": c, "runtime_id": runtime, "workflow_step": "todo"})
	claim := func() *db.AgentTaskQueue {
		t.Helper()
		row, err := testHandler.TaskService.ClaimTask(ctx, parseUUID(agent))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	if claim() != nil {
		t.Fatal("claimed before any required input")
	}
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, a)
	if claim() != nil {
		t.Fatal("claimed after only A")
	}
	dbfx.Exec(t, `UPDATE issue SET status='in_review' WHERE id=$1`, b)
	if claim() != nil {
		t.Fatal("in_review silently accepted B")
	}
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, b)
	var wg sync.WaitGroup
	results := make(chan *db.AgentTaskQueue, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			row, err := testHandler.TaskService.ClaimTask(ctx, parseUUID(agent))
			results <- row
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for row := range results {
		if row != nil {
			count++
			if uuidToString(row.ID) != run {
				t.Fatal("new logical run created")
			}
		}
	}
	if count != 1 {
		t.Fatalf("concurrent claims=%d", count)
	}
	// A reopened source fences start, including an already-delivered claim.
	dbfx.Exec(t, `UPDATE issue SET status='in_progress' WHERE id=$1`, a)
	if _, err := testHandler.TaskService.StartTask(ctx, parseUUID(run)); err == nil {
		t.Fatal("stale readiness started after reopen")
	}
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, a)
	if _, err := testHandler.TaskService.StartTask(ctx, parseUUID(run)); err != nil {
		t.Fatal(err)
	}
	// An edit cannot weaken or replace a running stage's inputs.
	d := dbfx.Issue(t, "D")
	testutil.Call(t, testHandler.CreateIssueDependency, relationRequest("POST", c, "", map[string]any{"issue_id": d})).Want(409)
}

func TestIssueDependencyHumanAcceptanceAndRemovalHistory(t *testing.T) {
	source := dbfx.Issue(t, "delivered source", testutil.Cols{"status": "in_review"})
	target := dbfx.Issue(t, "review dependent")
	runtime := dbfx.Runtime(t, "acceptance-runtime")
	agent := dbfx.Agent(t, "acceptance-agent", runtime)
	dbfx.Task(t, agent, testutil.Cols{"issue_id": source, "runtime_id": runtime, "status": "completed", "result": testutil.Raw(`'{"workflow_evidence":{"source_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}'::jsonb`)})
	id := addDependency(t, target, source, "accepted")
	list := func() bool {
		t.Helper()
		var result struct {
			Ready bool `json:"ready"`
		}
		testutil.Call(t, testHandler.ListIssueDependencies, relationRequest("GET", target, "", nil)).Want(200).JSON(&result)
		return result.Ready
	}
	if list() {
		t.Fatal("delivery skipped human acceptance")
	}
	version, err := testHandler.Queries.IssueEvidenceVersion(context.Background(), parseUUID(source))
	if err != nil {
		t.Fatal(err)
	}
	var revision int64
	dbfx.QueryRow(t, `SELECT revision FROM issue WHERE id=$1`, source).Scan(&revision)
	r := relationParam(relationRequest("POST", target, "/"+id+"/accept", map[string]any{"revision": revision, "evidence_version": version}), "relationID", id)
	r.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.AcceptIssueDependency, r).Want(403)
	r = relationParam(relationRequest("POST", target, "/"+id+"/accept", map[string]any{"revision": revision, "evidence_version": version}), "relationID", id)
	testutil.Call(t, testHandler.AcceptIssueDependency, r).Want(200)
	if !list() {
		t.Fatal("human acceptance not applied")
	}
	// A new code/report revision can arrive without editing issue.revision.
	// The old acceptance and a stale approval click must both stop applying.
	dbfx.Task(t, agent, testutil.Cols{"issue_id": source, "runtime_id": runtime, "status": "completed", "result": testutil.Raw(`'{"workflow_evidence":{"source_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}'::jsonb`)})
	if list() {
		t.Fatal("old acceptance applied to a new SHA")
	}
	r = relationParam(relationRequest("POST", target, "/"+id+"/accept", map[string]any{"revision": revision, "evidence_version": version}), "relationID", id)
	testutil.Call(t, testHandler.AcceptIssueDependency, r).Want(409)
	version, err = testHandler.Queries.IssueEvidenceVersion(context.Background(), parseUUID(source))
	if err != nil {
		t.Fatal(err)
	}
	r = relationParam(relationRequest("POST", target, "/"+id+"/accept", map[string]any{"revision": revision, "evidence_version": version}), "relationID", id)
	testutil.Call(t, testHandler.AcceptIssueDependency, r).Want(200)
	if !list() {
		t.Fatal("current evidence could not be accepted")
	}
	dbfx.Exec(t, `UPDATE issue SET revision=revision+1,status='in_progress' WHERE id=$1`, source)
	if list() {
		t.Fatal("old acceptance survived reopen/new revision")
	}
	for range 2 {
		r = relationParam(relationRequest("DELETE", source, "/"+id, nil), "relationID", id)
		testutil.Call(t, testHandler.RemoveIssueDependency, r).Want(200)
	}
	if !list() {
		t.Fatal("removed dependency still gates")
	}
	var rows struct {
		Relations []json.RawMessage `json:"relations"`
	}
	testutil.Call(t, testHandler.ListIssueDependencies, relationRequest("GET", target, "?include_removed=true", nil)).Want(200).JSON(&rows)
	if len(rows.Relations) != 1 {
		t.Fatalf("lost relation history: %s", rows.Relations)
	}
}

func TestIssueDependencyNamedRunDoesNotCompleteItsParent(t *testing.T) {
	source := dbfx.Issue(t, "infrastructure", testutil.Cols{"status": "in_progress"})
	target := dbfx.Issue(t, "configure HTTPS")
	runtime := dbfx.Runtime(t, "named-result-runtime")
	agent := dbfx.Agent(t, "named-result-agent", runtime)
	run := dbfx.Task(t, agent, testutil.Cols{"issue_id": source, "runtime_id": runtime, "status": "running", "workflow_step": "in_progress"})
	var relation struct {
		ID string `json:"id"`
	}
	testutil.Call(t, testHandler.CreateIssueDependency, relationRequest("POST", target, "", map[string]any{"issue_id": source, "condition": map[string]any{"kind": "run_completed", "run_id": run, "source_step": "in_progress"}})).Want(201).JSON(&relation)
	ready := func() bool {
		t.Helper()
		ok, err := testHandler.Queries.IssueDependenciesReady(context.Background(), db.IssueDependenciesReadyParams{IssueID: parseUUID(target), Step: "todo"})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if ready() {
		t.Fatal("unfinished source run satisfied the gate")
	}
	for range 2 {
		if _, err := testHandler.TaskService.CompleteTask(context.Background(), parseUUID(run), []byte(`{"output":"infrastructure readiness delivered"}`), "", "", "", false, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if !ready() {
		t.Fatal("delivered result did not unblock configuration")
	}
	if issueStatusForTest(t, source) != "in_progress" {
		t.Fatal("a delivered result completed the parent issue")
	}
	// Moving a run's current guard does not rewrite the entry step evidence.
	dbfx.Exec(t, `UPDATE agent_task_queue SET workflow_step='in_review' WHERE id=$1`, run)
	if !ready() {
		t.Fatal("a mutable run guard rewrote historical result scope")
	}
}

func TestIssueDependencyInfrastructureIntegrationHasNoCycleOrImplicitAcceptance(t *testing.T) {
	app := dbfx.Issue(t, "application final acceptance", testutil.Cols{"status": "in_progress"})
	infra := dbfx.Issue(t, "infrastructure readiness", testutil.Cols{"parent_issue_id": app, "status": "in_review"})
	https := dbfx.Issue(t, "HTTPS configuration", testutil.Cols{"parent_issue_id": app, "status": "in_progress"})
	auth := dbfx.Issue(t, "passkey configuration", testutil.Cols{"parent_issue_id": app, "status": "in_progress"})
	addDependency(t, https, infra, "in_review")
	addDependency(t, auth, infra, "in_review")
	addDependency(t, app, https, "accepted")
	addDependency(t, app, auth, "accepted")
	for _, id := range []string{https, auth} {
		ready, err := testHandler.Queries.IssueDependenciesReady(context.Background(), db.IssueDependenciesReadyParams{IssueID: parseUUID(id), Step: "in_progress"})
		if err != nil || !ready {
			t.Fatalf("infrastructure delivery did not unblock configuration: ready=%v err=%v", ready, err)
		}
	}
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id IN ($1,$2)`, https, auth)
	linkMergedGitHubPRForTest(t, app, "dependency-partial-integration")
	testHandler.maybeAutoCompleteIssue(context.Background(), parseUUID(testWorkspaceID), parseUUID(app), nil)
	if issueStatusForTest(t, app) != "in_progress" {
		t.Fatal("merge bypassed required human acceptances")
	}
	// Machine traffic can record context, but cannot remove a required gate.
	var id string
	dbfx.QueryRow(t, `SELECT id::text FROM issue_dependency WHERE issue_id=$1 AND depends_on_issue_id=$2`, app, https).Scan(&id)
	r := relationParam(relationRequest("DELETE", app, "/"+id, nil), "relationID", id)
	r.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.RemoveIssueDependency, r).Want(403)
}

func TestIssueDependencyTenantIsolation(t *testing.T) {
	a := dbfx.Issue(t, "visible")
	workspace := dbfx.Workspace(t, "foreign", "dependency-foreign-"+workflowTestSuffix())
	b := dbfx.Issue(t, "private title must not leak", testutil.Cols{"workspace_id": workspace})
	testutil.Call(t, testHandler.CreateIssueDependency, relationRequest("POST", a, "", map[string]any{"issue_id": b})).Want(404)
	// Even a historical cross-workspace relation is not projected.
	dbfx.Insert(t, "issue_dependency", testutil.Cols{"issue_id": a, "depends_on_issue_id": b, "type": "related"})
	var result struct {
		Relations []json.RawMessage `json:"relations"`
	}
	testutil.Call(t, testHandler.ListIssueDependencies, relationRequest("GET", a, "", nil)).Want(200).JSON(&result)
	if len(result.Relations) != 0 {
		t.Fatal("foreign relation exposed")
	}
	r := relationRequest("GET", b, "", nil)
	r.URL.RawQuery = "workspace_id=" + workspace
	testutil.Call(t, testHandler.ListIssueDependencies, r).Want(404)
}

func TestIssueDependencyLinearChainAndCancelledInput(t *testing.T) {
	a, b, c := dbfx.Issue(t, "A"), dbfx.Issue(t, "B"), dbfx.Issue(t, "C")
	addDependency(t, b, a, "done")
	addDependency(t, c, b, "done")
	ready := func(id string, expected bool) {
		t.Helper()
		actual, err := testHandler.Queries.IssueDependenciesReady(context.Background(), db.IssueDependenciesReadyParams{IssueID: parseUUID(id), Step: "todo"})
		if err != nil || actual != expected {
			t.Fatalf("chain readiness: issue=%s ready=%v expected=%v err=%v", id, actual, expected, err)
		}
	}
	ready(b, false)
	ready(c, false)
	dbfx.Exec(t, "UPDATE issue SET status='in_review' WHERE id=$1", a)
	ready(b, false)
	dbfx.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", a)
	ready(b, true)
	ready(c, false)
	dbfx.Exec(t, "UPDATE issue SET status='cancelled' WHERE id=$1", b)
	ready(c, false)
	dbfx.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", b)
	ready(c, true)
	dbfx.Exec(t, "UPDATE issue SET status='in_progress' WHERE id=$1", b)
	ready(c, false)
}
