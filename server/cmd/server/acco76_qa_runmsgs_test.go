package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// QA repro (ACCO-76): `multica issue run-messages` from inside an agent task
// reads another (non-chat) issue run's transcript.
func TestQA_TaskTokenReadsOtherIssueRunMessages(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtime := fx.Runtime(t, "qa-runs")
	agent := fx.Agent(t, "qa-runs", runtime)
	issue := fx.Issue(t, "QA run messages")
	earlier := fx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed"})
	fx.InsertNoID(t, "task_message", testutil.Cols{"task_id": earlier, "seq": 1, "type": "assistant", "content": "earlier ordinary run transcript"}, "task_id=$1", earlier)
	current := fx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "running"})
	token := mintAgentTaskToken(t, agent, current, testUserID)
	r, _ := http.NewRequest("GET", testServer.URL+"/api/tasks/"+earlier+"/messages", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Workspace-ID", testWorkspaceID)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("GET /api/tasks/{earlier issue run}/messages with task token -> %d %s", resp.StatusCode, body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "earlier ordinary run transcript") {
		t.Fatalf("issue run transcript hidden from agent: %d", resp.StatusCode)
	}
}

func TestQA_TaskTokenListsOrdinaryRunsWithoutOtherChatRuns(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtime := fx.Runtime(t, "qa-history")
	agent := fx.Agent(t, "qa-history", runtime)
	issue := fx.Issue(t, "QA history")
	ordinary := fx.Task(t, agent, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed"})
	makeChat := func() string {
		return fx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agent, "creator_id": testUserID, "explicitly_created_at": time.Now()})
	}
	own := fx.Task(t, agent, testutil.Cols{"chat_session_id": makeChat(), "runtime_id": runtime, "status": "running"})
	hidden := fx.Task(t, agent, testutil.Cols{"chat_session_id": makeChat(), "runtime_id": runtime, "status": "completed"})
	token := mintAgentTaskToken(t, agent, own, testUserID)
	r, _ := http.NewRequest("GET", testServer.URL+"/api/agents/"+agent+"/tasks?limit=1", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Workspace-ID", testWorkspaceID)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("agent history unavailable: %d %s", resp.StatusCode, body)
	}
	var tasks []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &tasks); err != nil || len(tasks) != 1 || tasks[0].ID != own {
		t.Fatalf("hidden chat shortened first page: %s %v", body, err)
	}
	if resp.Header.Get("X-Agent-Tasks-Next-Cursor") == "" {
		t.Fatal("ordinary history disappeared behind hidden chat")
	}
	r.URL.RawQuery = "limit=1&before=" + resp.Header.Get("X-Agent-Tasks-Next-Cursor")
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || json.Unmarshal(body, &tasks) != nil || len(tasks) != 1 || tasks[0].ID != ordinary {
		t.Fatalf("ordinary run missing from next page: %d %s", resp.StatusCode, body)
	}
	_ = hidden
}
