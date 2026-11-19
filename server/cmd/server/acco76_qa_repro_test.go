package main

import (
	"context"
	"github.com/gorilla/websocket"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// QA repro (ACCO-76): the agent running a legacy agent-chat task must be able
// to read the files the user attached to the message it is answering.
func TestQA_TaskTokenReadsUserChatAttachment(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtime := fx.Runtime(t, "qa-att")
	agent := fx.Agent(t, "qa-att", runtime)
	chat := fx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agent, "creator_id": testUserID, "explicitly_created_at": time.Now()})
	msg := fx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chat, "role": "user", "content": "see file"})
	att := fx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "chat_session_id": chat, "chat_message_id": msg, "uploader_type": "member", "uploader_id": testUserID, "filename": "input.txt", "url": "/uploads/qa-input.txt", "content_type": "text/plain", "size_bytes": 1})
	task := fx.Task(t, agent, testutil.Cols{"chat_session_id": chat, "runtime_id": runtime, "status": "running"})
	token := mintAgentTaskToken(t, agent, task, testUserID)
	r, _ := http.NewRequest("GET", testServer.URL+"/api/attachments/"+att, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Workspace-ID", testWorkspaceID)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("GET /api/attachments/{id} with own task token -> %d %s", resp.StatusCode, body)
	if resp.StatusCode != 200 {
		t.Fatalf("agent cannot read the user's attachment in its own chat: %d", resp.StatusCode)
	}
}

// QA repro (ACCO-76): a task with no issue and no chat (quick-create,
// run_only autopilot) is an ordinary workspace task. Its live task:* frames
// must still reach workspace members' sockets.
func TestQA_NoIssueNoChatTaskFrameDelivered(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtime := fx.Runtime(t, "qa-qc")
	agent := fx.Agent(t, "qa-qc", runtime)
	task := fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "context": `{"type":"quick_create"}`})
	frame := []byte(`{"type":"task:completed","payload":{"task_id":"` + task + `"},"task_id":"` + task + `","workspace_id":"` + testWorkspaceID + `"}`)
	ok, err := newScopeAuthorizer(db.New(testPool)).AuthorizeDelivery(context.Background(), testUserID, testWorkspaceID, frame)
	t.Logf("AuthorizeDelivery(task:completed for quick-create task) -> %v %v", ok, err)
	if err != nil || !ok {
		t.Fatalf("workspace member cannot receive quick-create task frame")
	}
}

func TestQA_NoIssueNoChatTaskFrameReachesSocket(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtime := fx.Runtime(t, "qa-qc2")
	agent := fx.Agent(t, "qa-qc2", runtime)
	task := fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "context": `{"type":"quick_create"}`})
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(testServer.URL, "http")+"/ws?workspace_id="+testWorkspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteJSON(map[string]any{"type": "auth", "payload": map[string]string{"token": testToken}})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, ack, err := conn.ReadMessage(); err != nil || !strings.Contains(string(ack), "auth_ack") {
		t.Fatalf("ack %s %v", ack, err)
	}
	time.Sleep(100 * time.Millisecond)
	testChatHub.BroadcastToWorkspace(testWorkspaceID, []byte(`{"type":"task:completed","payload":{"task_id":"`+task+`"},"task_id":"`+task+`","workspace_id":"`+testWorkspaceID+`"}`))
	testChatHub.BroadcastToWorkspace(testWorkspaceID, []byte(`{"type":"test:barrier","payload":{}}`))
	_, got, err := conn.ReadMessage()
	t.Logf("first frame after quick-create task:completed broadcast: %s", got)
	if err != nil || !strings.Contains(string(got), "task:completed") {
		t.Fatalf("quick-create task:completed dropped: %s %v", got, err)
	}
}
