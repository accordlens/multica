package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestIssueContextLinksExplicitReferencesAndCorrection(t *testing.T) {
	a, b := dbfx.Issue(t, "source"), dbfx.Issue(t, "Referenced input")
	var number int32
	dbfx.QueryRow(t, `SELECT number FROM issue WHERE id=$1`, b).Scan(&number)
	other, err := testHandler.Queries.GetIssueInWorkspace(context.Background(), db.GetIssueInWorkspaceParams{ID: parseUUID(b), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	description := testHandler.dependencyIssueIdentifier(context.Background(), other) + " https://multica.test/workspace/issues/" + b
	dbfx.Exec(t, `UPDATE issue SET description=$1 WHERE id=$2`, description, a)
	for range 3 {
		testutil.Call(t, testHandler.SyncIssueContextLinks, relationRequest("POST", a, "/sync", nil)).Want(200)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_dependency WHERE issue_id=$1`, a); n != 1 {
		t.Fatalf("duplicate explicit reference created %d links", n)
	}
	var id string
	dbfx.QueryRow(t, `SELECT id::text FROM issue_dependency WHERE issue_id=$1`, a).Scan(&id)
	testutil.Call(t, testHandler.RemoveIssueDependency, relationParam(relationRequest("DELETE", a, "/"+id, nil), "relationID", id)).Want(200)
	testutil.Call(t, testHandler.SyncIssueContextLinks, relationRequest("POST", a, "/sync", nil)).Want(200)
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_dependency WHERE issue_id=$1 AND removed_at IS NULL`, a); n != 0 {
		t.Fatal("replay undid a correction")
	}
	// Similar titles and unknown identifiers are not linking evidence.
	c := dbfx.Issue(t, "Referenced input", testutil.Cols{"description": "Referenced input and FOREIGN-99999"})
	testutil.Call(t, testHandler.SyncIssueContextLinks, relationRequest("POST", c, "/sync", nil)).Want(200)
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_dependency WHERE issue_id=$1`, c); n != 0 {
		t.Fatal("guessed a relation from text similarity")
	}
}

func TestIssueContextLinkEmitsOnlyMetadataInvalidation(t *testing.T) {
	bus := testHandler.Bus
	testHandler.Bus = events.New()
	t.Cleanup(func() { testHandler.Bus = bus })
	changes, ordinary := 0, 0
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(events.Event) { ordinary++ })
	testHandler.Bus.Subscribe(protocol.EventIssueRelationsChanged, func(e events.Event) {
		changes++
		ids := e.Payload.(map[string]any)["issue_ids"].([]string)
		if len(ids) != 2 {
			t.Errorf("reciprocal invalidation IDs=%v", ids)
		}
	})
	a, b := dbfx.Issue(t, "linked A"), dbfx.Issue(t, "linked B")
	testutil.Call(t, testHandler.CreateIssueDependency, relationRequest("POST", a, "", map[string]any{"issue_id": b, "type": "related"})).Want(201)
	if changes != 1 || ordinary != 0 {
		t.Fatalf("context link emitted changes=%d ordinary issue updates=%d", changes, ordinary)
	}
}

func TestIssueProvenanceNativeEvidenceAutomaticallyLinksCommitAndRetainsCorrection(t *testing.T) {
	issue := dbfx.Issue(t, "native code evidence", testutil.Cols{"status": "in_review"})
	runtime := dbfx.Runtime(t, "native-evidence-runtime")
	agent := dbfx.Agent(t, "native-evidence-agent", runtime)
	run := dbfx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed"})
	raw, _ := json.Marshal(map[string]any{"workflow_evidence": map[string]any{"run_id": run, "source": map[string]string{"sha": strings.Repeat("a", 40), "dirty_hash": strings.Repeat("b", 64)}, "finished_at": time.Now().UTC(), "outcome": "assertion_failed"}})
	dbfx.Exec(t, `UPDATE agent_task_queue SET result=$1::jsonb WHERE id=$2`, string(raw), run)
	list := func() []IssueProvenanceLink {
		t.Helper()
		var out struct {
			Links []IssueProvenanceLink `json:"links"`
		}
		testutil.Call(t, testHandler.ListIssueProvenance, relationRequest("GET", issue, "", nil)).Want(200).JSON(&out)
		return out.Links
	}
	links := list()
	if len(links) != 1 || links[0].Source != "workflow_evidence" || links[0].RunID != run || links[0].CommitSHA != strings.Repeat("a", 40) || links[0].DirtyHash != strings.Repeat("b", 64) {
		t.Fatalf("wrong native source mapping: %+v", links)
	}
	for range 2 {
		testutil.Call(t, testHandler.RemoveIssueProvenance, relationParam(relationRequest("POST", issue, "", map[string]any{"run_id": run}), "linkID", links[0].ID)).Want(200)
	}
	if len(list()) != 0 {
		t.Fatal("reconnect restored a corrected source pointer")
	}
	var retained string
	dbfx.QueryRow(t, `SELECT result->'workflow_evidence'->>'outcome' FROM agent_task_queue WHERE id=$1`, run).Scan(&retained)
	if retained != "assertion_failed" {
		t.Fatal("correcting a link changed an authoritative result")
	}
}

func TestIssueProvenanceRevisionHistoryReplayAndACL(t *testing.T) {
	issue := dbfx.Issue(t, "versioned code", testutil.Cols{"status": "test"})
	runtime := dbfx.Runtime(t, "provenance-runtime")
	agent := dbfx.Agent(t, "provenance-agent", runtime)
	run := dbfx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed", "workflow_step": "review", "context": testutil.Raw(`'{"workflow_entry_step":"test","hybrid_workflow":{"version":"frozen"}}'::jsonb`), "result": testutil.Raw(`'{"workflow_evidence":{"source_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","report_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}'::jsonb`)})
	var linkID string
	for _, sha := range []string{strings.Repeat("a", 40), strings.Repeat("a", 40), strings.Repeat("c", 40)} {
		var out struct {
			ID string `json:"id"`
		}
		testutil.Call(t, testHandler.CreateIssueProvenance, relationRequest("POST", issue, "", map[string]any{"run_id": run, "kind": "commit", "commit_sha": sha})).Want(200).JSON(&out)
		linkID = out.ID
	}
	list := func(history bool) struct {
		Links     []IssueProvenanceLink `json:"links"`
		Runs      []map[string]any      `json:"runs"`
		Artifacts []map[string]any      `json:"artifacts"`
	} {
		t.Helper()
		var out struct {
			Links     []IssueProvenanceLink `json:"links"`
			Runs      []map[string]any      `json:"runs"`
			Artifacts []map[string]any      `json:"artifacts"`
		}
		suffix := ""
		if history {
			suffix = "?include_removed=true"
		}
		testutil.Call(t, testHandler.ListIssueProvenance, relationRequest("GET", issue, suffix, nil)).Want(200).JSON(&out)
		return out
	}
	out := list(false)
	if len(out.Links) != 2 || out.Links[0].Step != "test" || len(out.Runs) != 1 {
		t.Fatalf("wrong run/revision mapping: %+v", out)
	}
	if out.Runs[0]["workflow_evidence"] == nil {
		t.Fatal("native evidence not linked")
	}
	var frozen string
	dbfx.QueryRow(t, `SELECT context->'hybrid_workflow'->>'version' FROM agent_task_queue WHERE id=$1`, run).Scan(&frozen)
	if frozen != "frozen" {
		t.Fatal("reporting a link rewrote the execution snapshot")
	}
	for range 2 {
		testutil.Call(t, testHandler.RemoveIssueProvenance, relationParam(relationRequest("POST", issue, "", map[string]any{"run_id": run}), "linkID", linkID)).Want(200)
	}
	if len(list(false).Links) != 1 || len(list(true).Links) != 2 {
		t.Fatal("correction lost revision history")
	}
	foreign := dbfx.Issue(t, "another issue")
	testutil.Call(t, testHandler.CreateIssueProvenance, relationRequest("POST", foreign, "", map[string]any{"run_id": run, "kind": "commit", "commit_sha": strings.Repeat("a", 40)})).Want(404)
	// Artifact names from another workspace, private chats or source snapshots
	// never enter the projection, even with corrupt historical pointers.
	workspace := dbfx.Workspace(t, "private artifacts", "private-artifacts-"+workflowTestSuffix())
	file := dbfx.Insert(t, "attachment", testutil.Cols{"workspace_id": workspace, "issue_id": issue, "uploader_type": "member", "uploader_id": testUserID, "filename": "private-file", "url": "private-key", "content_type": "text/plain", "size_bytes": 1})
	testutil.Call(t, testHandler.CreateIssueProvenance, relationRequest("POST", issue, "", map[string]any{"run_id": run, "kind": "artifact", "artifact_id": file, "sha256": strings.Repeat("d", 64)})).Want(404)
	if len(list(false).Artifacts) != 0 {
		t.Fatal("foreign artifact metadata leaked")
	}
}

func TestIssueProvenancePRCanLinkMultipleTasksWithoutWebhook(t *testing.T) {
	a, b := dbfx.Issue(t, "PR task A"), dbfx.Issue(t, "PR task B")
	linkMergedGitHubPRForTest(t, a, "provenance-shared")
	runtime := dbfx.Runtime(t, "provenance-pr-runtime")
	agent := dbfx.Agent(t, "provenance-pr-agent", runtime)
	for _, issue := range []string{a, b} {
		run := dbfx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed"})
		for range 2 {
			testutil.Call(t, testHandler.CreateIssueProvenance, relationRequest("POST", issue, "", map[string]any{"run_id": run, "kind": "pull_request", "url": "https://github.com/acme/provenance-shared/pull/1"})).Want(200)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request ipr JOIN github_pull_request p ON p.id=ipr.pull_request_id WHERE p.repo_name='provenance-shared'`); n != 2 {
		t.Fatalf("PR backlinks = %d, want two distinct tasks", n)
	}
	if issueStatusForTest(t, b) != "todo" {
		t.Fatal("a provenance pointer completed the task")
	}
	// An unmirrored PR is still an explicit source pointer on a private pilot.
	run := dbfx.Task(t, agent, testutil.Cols{"issue_id": b, "runtime_id": runtime, "status": "completed"})
	testutil.Call(t, testHandler.CreateIssueProvenance, relationRequest("POST", b, "", map[string]any{"run_id": run, "kind": "pull_request", "url": "https://github.com/acme/provenance-shared/pull/2"})).Want(200)
}

func TestIssueProvenanceRetainsChatPrivacyAfterLivePointersDisappear(t *testing.T) {
	issue, dependent := dbfx.Issue(t, "public sources"), dbfx.Issue(t, "dependent")
	runtime := dbfx.Runtime(t, "protected-source-runtime")
	agent := dbfx.Agent(t, "protected-source-agent", runtime)
	visible := dbfx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed"})
	private := dbfx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed"})
	unknown := dbfx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed"})
	unclassified := dbfx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed"})
	for _, run := range []string{visible, private, unknown, unclassified} {
		raw := nativeSourceResult(t, run, strings.Repeat("a", 40), "", "passed")
		dbfx.Exec(t, `UPDATE agent_task_queue SET result=$1::jsonb WHERE id=$2`, string(raw), run)
	}
	// The live chat FK is already NULL. Retained private/unknown origins and a
	// missing classification must still be denied by the issue-facing API.
	dbfx.Exec(t, `UPDATE chat_protected_task SET is_private=true,chat_session_id=$1 WHERE task_id=$2`, "00000000-0000-0000-0000-000000000001", private)
	dbfx.Exec(t, `UPDATE chat_protected_task SET is_private=true,chat_session_id=NULL WHERE task_id=$1`, unknown)
	dbfx.Exec(t, `DELETE FROM chat_protected_task WHERE task_id=$1`, unclassified)
	for _, run := range []string{private, unknown, unclassified} {
		testutil.Call(t, testHandler.CreateIssueProvenance, relationRequest("POST", issue, "", map[string]any{"run_id": run, "kind": "commit", "commit_sha": strings.Repeat("b", 40)})).Want(404)
		testutil.Call(t, testHandler.CreateIssueDependency, relationRequest("POST", dependent, "", map[string]any{"issue_id": issue, "condition": map[string]string{"kind": "run_completed", "run_id": run}})).Want(404)
	}
	file := dbfx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issue, "task_id": visible, "uploader_type": "member", "uploader_id": testUserID, "filename": "retained-private-file", "url": "retained-private-key-" + workflowTestSuffix(), "content_type": "text/plain", "size_bytes": 1})
	body := map[string]any{"run_id": visible, "kind": "artifact", "artifact_id": file, "sha256": strings.Repeat("d", 64)}
	testutil.Call(t, testHandler.CreateIssueProvenance, relationRequest("POST", issue, "", body)).Want(200)
	dbfx.Exec(t, `INSERT INTO chat_protected_object (url,attachment_id) SELECT url,id FROM attachment WHERE id=$1`, file)
	testutil.Call(t, testHandler.CreateIssueProvenance, relationRequest("POST", issue, "", body)).Want(404)
	var out struct {
		Links     []IssueProvenanceLink `json:"links"`
		Runs      []map[string]any      `json:"runs"`
		Artifacts []map[string]any      `json:"artifacts"`
	}
	testutil.Call(t, testHandler.ListIssueProvenance, relationRequest("GET", issue, "", nil)).Want(200).JSON(&out)
	if len(out.Runs) != 1 || out.Runs[0]["run_id"] != visible || len(out.Links) != 1 || out.Links[0].RunID != visible || out.Links[0].Kind != "commit" || len(out.Artifacts) != 0 {
		t.Fatalf("protected run or file metadata leaked: %+v", out)
	}
}
