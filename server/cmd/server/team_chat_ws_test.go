package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestTeamChat_WSOpenRevokeReconnectAndRetainedFrame(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	id := fx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "creator_id": testUserID, "kind": "private_channel", "name": "ws-" + uuid.NewString()})
	fx.InsertNoID(t, "chat_participant", testutil.Cols{"chat_session_id": id, "actor_type": "member", "actor_id": testUserID, "role": "manager"}, "chat_session_id=$1", id)
	q := db.New(testPool)
	gate := newScopeAuthorizer(q)
	fb := &fakeBroadcaster{}
	bus := events.New()
	registerListeners(bus, fb, q)
	retained := events.Event{Type: "chat:message", WorkspaceID: testWorkspaceID, ChatSessionID: id, Payload: map[string]any{"chat_session_id": id, "content": "private secret"}}
	bus.Publish(retained)
	if len(fb.workspaceCalls) != 0 || len(fb.userCalls) != 1 || fb.userCalls[0].userID != testUserID {
		t.Fatalf("routing leaked: %+v", fb)
	}
	frame := fb.userCalls[0].msg
	open := func() *websocket.Conn {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(testServer.URL, "http")+"/ws?workspace_id="+testWorkspaceID, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if err = conn.WriteJSON(map[string]any{"type": "auth", "payload": map[string]string{"token": testToken}}); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, ack, err := conn.ReadMessage()
		if err != nil || !strings.Contains(string(ack), "auth_ack") {
			t.Fatalf("ack %s %v", ack, err)
		}
		return conn
	}
	conn := open()
	if err := conn.WriteJSON(map[string]any{"type": "subscribe", "payload": map[string]string{"scope": "chat", "id": id}}); err != nil {
		t.Fatal(err)
	}
	_, ack, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(ack), "subscribe_ack") {
		t.Fatalf("subscribe %s %v", ack, err)
	}
	testChatHub.SendToUser(testUserID, frame)
	_, actual, readErr := conn.ReadMessage()
	if readErr != nil || !strings.Contains(string(actual), "private secret") {
		t.Fatalf("legacy live delivery lost: %s %v", actual, readErr)
	}
	fx.Exec(t, "UPDATE chat_participant SET revoked_at=now() WHERE chat_session_id=$1", id)
	fx.Exec(t, "UPDATE chat_session SET acl_version=acl_version+1 WHERE id=$1", id)
	// This is the same retained frame a relay would have captured before revoke.
	if allowed, err := gate.AuthorizeDelivery(context.Background(), testUserID, testWorkspaceID, frame); err != nil || allowed {
		t.Fatalf("retained content authorized after revoke: %v %v", allowed, err)
	}
	testChatHub.SendToUser(testUserID, frame)
	testChatHub.SendToUser(testUserID, []byte(`{"type":"test:barrier","payload":{}}`))
	_, actual, readErr = conn.ReadMessage()
	if readErr != nil || strings.Contains(string(actual), "private secret") || !strings.Contains(string(actual), "test:barrier") {
		t.Fatalf("queued private delivery after revoke: %s %v", actual, readErr)
	}
	if err = conn.WriteJSON(map[string]any{"type": "subscribe", "payload": map[string]string{"scope": "chat", "id": id}}); err != nil {
		t.Fatal(err)
	}
	_, ack, err = conn.ReadMessage()
	if err != nil || !strings.Contains(string(ack), "forbidden") {
		t.Fatalf("open revoked socket granted %s %v", ack, err)
	}
	conn.Close()
	reconnect := open()
	if err = reconnect.WriteJSON(map[string]any{"type": "subscribe", "payload": map[string]string{"scope": "chat", "id": id}}); err != nil {
		t.Fatal(err)
	}
	_, ack, err = reconnect.ReadMessage()
	if err != nil || !strings.Contains(string(ack), "forbidden") {
		t.Fatalf("reconnect granted %s %v", ack, err)
	}
	bus.Publish(retained)
	if len(fb.userCalls) != 1 {
		t.Fatal("revoked recipient received later delivery")
	}
	var top map[string]any
	json.Unmarshal(frame, &top)
	if top["conversation_id"] != id {
		t.Fatal("frame lost delivery ACL descriptor")
	}
	// Workspace peers/admins cannot subscribe by a copied direct ID either.
	peer := fx.User(t, "WS Admin", "ws-admin-"+uuid.NewString()+"@test.invalid")
	fx.Member(t, testWorkspaceID, peer, "admin")
	if allowed, err := gate.AuthorizeScope(context.Background(), peer, testWorkspaceID, realtime.ScopeChat, id); err != nil || allowed {
		t.Fatalf("admin private bypass %v %v", allowed, err)
	}
	if _, err := q.GetChatAccess(context.Background(), db.GetChatAccessParams{UserID: util.MustParseUUID(peer), WorkspaceID: util.MustParseUUID(testWorkspaceID), ChatSessionID: util.MustParseUUID(id)}); err == nil {
		t.Fatal("peer ACL unexpectedly visible")
	}
}

func TestTeamChat_HTTPTaskTokenAndIdentitySpoof(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtime := fx.Runtime(t, "token")
	agent := fx.Agent(t, "token", runtime)
	makeChat := func() string {
		return fx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agent, "creator_id": testUserID, "explicitly_created_at": time.Now()})
	}
	own, other := makeChat(), makeChat()
	task := fx.Task(t, agent, testutil.Cols{"chat_session_id": own, "runtime_id": runtime})
	token := mintAgentTaskToken(t, agent, task, testUserID)
	do := func(path, credential string, want int) {
		t.Helper()
		r, _ := http.NewRequest("GET", testServer.URL+path, nil)
		r.Header.Set("X-User-ID", testUserID)
		r.Header.Set("X-Agent-ID", agent)
		r.Header.Set("X-Task-ID", task)
		r.Header.Set("X-Workspace-ID", testWorkspaceID)
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("%s got %d want %d: %s", path, response.StatusCode, want, body)
		}
	}
	do("/api/chat/v2/conversations/"+own, token, 200)
	do("/api/chat/v2/conversations/"+other, token, 404)
	otherTask := fx.Task(t, agent, testutil.Cols{"chat_session_id": other, "runtime_id": runtime})
	do("/api/tasks/"+otherTask+"/messages", token, 404)
	do("/api/agents/"+agent+"/tasks", token, 403)
	do("/api/agent-task-snapshot", token, 403)
	do("/api/chat/sessions", token, 403)
	do("/api/chat/pending-tasks", token, 403)
	do("/api/chat/pinned-agents", token, 403)
	do("/api/chat/v2/conversations/"+other, "", 401)
	fx.Exec(t, "UPDATE agent_task_queue SET status='completed' WHERE id=$1", task)
	do("/api/chat/v2/conversations/"+own, token, 404)
}

func TestTeamChat_TaskScopeSurvivesConversationDeletion(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtime := fx.Runtime(t, "scope")
	agent := fx.Agent(t, "scope", runtime, testutil.Cols{"visibility": "workspace"})
	conversation := fx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agent, "creator_id": testUserID, "explicitly_created_at": time.Now()})
	issue := fx.Issue(t, "Task scope fixture")
	task := fx.Task(t, agent, testutil.Cols{"chat_session_id": conversation, "runtime_id": runtime, "issue_id": issue, "context": `{"input":"private-task-input"}`, "result": `{"content":"private-task-result"}`})
	fx.InsertNoID(t, "task_message", testutil.Cols{"task_id": task, "seq": 1, "type": "assistant", "content": "private-task-transcript"}, "task_id=$1", task)
	q := db.New(testPool)
	gate := newScopeAuthorizer(q)
	peer := fx.User(t, "Scope admin", "scope-admin-"+uuid.NewString()+"@test.invalid")
	fx.Member(t, testWorkspaceID, peer, "admin")
	peerToken, err := generateTestJWT(peer, "scope-admin@test.invalid", "Scope admin")
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, token string, want int, absent bool) {
		t.Helper()
		r, _ := http.NewRequest("GET", testServer.URL+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s: status=%d want=%d body=%s error=%v", path, resp.StatusCode, want, body, err)
		}
		if absent && (strings.Contains(string(body), task) || strings.Contains(string(body), "private-task-")) {
			t.Fatalf("%s leaked protected task: %s", path, body)
		}
	}
	request("/api/tasks/"+task+"/messages", testToken, 200, false)
	request("/api/tasks/"+task+"/messages", peerToken, 404, true)
	allowed, err := gate.AuthorizeDelivery(context.Background(), testUserID, testWorkspaceID, []byte(`{"type":"task:message","workspace_id":"`+testWorkspaceID+`","conversation_id":"`+uuid.NewString()+`","task_id":"`+task+`"}`))
	if err != nil || allowed {
		t.Fatalf("mismatched delivery scope allowed: %v %v", allowed, err)
	}
	fx.Exec(t, "DELETE FROM chat_session WHERE id=$1", conversation)
	var unbound bool
	var retained string
	if err := testPool.QueryRow(context.Background(), `SELECT t.chat_session_id IS NULL,p.chat_session_id::text FROM agent_task_queue t JOIN chat_protected_task p ON p.task_id=t.id AND p.is_private WHERE t.id=$1`, task).Scan(&unbound, &retained); err != nil || !unbound || retained != conversation {
		t.Fatalf("scope lost across FK deletion: %v %s %v", unbound, retained, err)
	}
	for _, token := range []string{testToken, peerToken} {
		request("/api/tasks/"+task+"/messages", token, 404, true)
		request("/api/agents/"+agent+"/tasks", token, 200, true)
		request("/api/agent-task-snapshot", token, 200, true)
		request("/api/issues/"+issue+"/task-runs", token, 200, true)
		request("/api/issues/"+issue+"/task-runs?scope=family", token, 200, true)
		request("/api/issues/"+issue+"/active-task", token, 200, true)
	}
	for _, user := range []string{testUserID, peer} {
		for _, scope := range []string{"", conversation} {
			frame, _ := json.Marshal(map[string]string{"type": "task:message", "workspace_id": testWorkspaceID, "task_id": task, "conversation_id": scope})
			allowed, err := gate.AuthorizeDelivery(context.Background(), user, testWorkspaceID, frame)
			if err != nil || allowed {
				t.Fatalf("deleted task delivery allowed: user=%s scope=%s allowed=%v error=%v", user, scope, allowed, err)
			}
		}
	}
}

func TestTeamChat_HTTPCopiedTicketRejectsSpoofedIdentity(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	id := fx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "creator_id": testUserID, "kind": "private_channel", "name": "file-" + uuid.NewString()})
	fx.InsertNoID(t, "chat_participant", testutil.Cols{"chat_session_id": id, "actor_type": "member", "actor_id": testUserID, "role": "manager"}, "chat_session_id=$1", id)
	att := fx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "chat_session_id": id, "uploader_type": "member", "uploader_id": testUserID, "filename": "secret.txt", "url": "/uploads/legacy-private.txt", "content_type": "text/plain", "size_bytes": 1})
	response := authRequest(t, "GET", "/api/attachments/"+att, nil)
	var metadata struct {
		DownloadURL string `json:"download_url"`
	}
	readJSON(t, response, &metadata)
	if !strings.Contains(metadata.DownloadURL, "chat_sig") {
		t.Fatal("missing identity-bound ticket")
	}
	peer := fx.User(t, "Ticket peer", "ticket-peer-"+uuid.NewString()+"@test.invalid")
	fx.Member(t, testWorkspaceID, peer, "admin")
	peerToken, err := generateTestJWT(peer, "ticket-peer@test.invalid", "Peer")
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{"", peerToken} {
		r, _ := http.NewRequest("GET", testServer.URL+metadata.DownloadURL, nil)
		r.Header.Set("X-User-ID", testUserID)
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Workspace-ID", testWorkspaceID)
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatalf("copied ticket/spoof got %d: %s", response.StatusCode, body)
		}
	}
}
