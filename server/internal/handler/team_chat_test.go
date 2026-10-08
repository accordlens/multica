package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/chataccess"
	"github.com/multica-ai/multica/server/internal/storage"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func teamConversation(t *testing.T, kind string) string {
	t.Helper()
	id := dbfx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "creator_id": testUserID, "kind": kind, "name": "test-" + uuid.NewString()})
	dbfx.InsertNoID(t, "chat_participant", testutil.Cols{"chat_session_id": id, "actor_type": "member", "actor_id": testUserID, "role": "manager"}, "chat_session_id=$1", id)
	return id
}
func teamRequest(t *testing.T, method, path, user, id string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("X-User-ID", user)
	r = withChatTestWorkspaceCtx(t, r)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("conversationId", id)
	rc.URLParams.Add("sessionId", id)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
	return r
}

func TestTeamChat_ConversationACL(t *testing.T) {
	peer := dbfx.User(t, "Team Peer", "team-peer-"+uuid.NewString()+"@test.invalid")
	dbfx.Member(t, testWorkspaceID, peer, "admin")
	for _, kind := range []string{"public_channel", "private_channel", "dm", "self_dm", "group_dm"} {
		t.Run(kind, func(t *testing.T) {
			id := teamConversation(t, kind)
			expected := 404
			if kind == "public_channel" {
				expected = 200
			}
			testutil.Call(t, testHandler.GetChatConversationV2, teamRequest(t, "GET", "/", peer, id)).Want(expected)
			testutil.Call(t, testHandler.GetChatConversationV2, teamRequest(t, "GET", "/", testUserID, id)).Want(200)
			// Legacy routes cannot expose a human conversation, even to its creator.
			testutil.Call(t, testHandler.GetChatSession, teamRequest(t, "GET", "/", testUserID, id)).Want(404)
			outsider := dbfx.User(t, "Outside", "outside-"+uuid.NewString()+"@test.invalid")
			testutil.Call(t, testHandler.ListChatMessagesV2, teamRequest(t, "GET", "/", outsider, id)).Want(404)
			dbfx.Exec(t, "UPDATE chat_participant SET revoked_at=now() WHERE chat_session_id=$1", id)
			want := 404
			if kind == "public_channel" {
				want = 200
			} // public still readable as workspace member
			testutil.Call(t, testHandler.GetChatConversationV2, teamRequest(t, "GET", "/", testUserID, id)).Want(want)
		})
	}
}

func TestTeamChat_DeactivatedAndRemovedMember(t *testing.T) {
	id := teamConversation(t, "private_channel")
	peer := dbfx.User(t, "Removed", "removed-"+uuid.NewString()+"@test.invalid")
	member := dbfx.Member(t, testWorkspaceID, peer, "member")
	dbfx.InsertNoID(t, "chat_participant", testutil.Cols{"chat_session_id": id, "actor_type": "member", "actor_id": peer}, "chat_session_id=$1 AND actor_id=$2", id, peer)
	dbfx.Exec(t, "UPDATE \"user\" SET deactivated_at=now() WHERE id=$1", peer)
	testutil.Call(t, testHandler.GetChatConversationV2, teamRequest(t, "GET", "/", peer, id)).Want(404)
	dbfx.Exec(t, "UPDATE \"user\" SET deactivated_at=NULL WHERE id=$1", peer)
	dbfx.Exec(t, "DELETE FROM member WHERE id=$1", member)
	testutil.Call(t, testHandler.GetChatConversationV2, teamRequest(t, "GET", "/", peer, id)).Want(404)
}

func TestTeamChat_CancelRechecksACLAfterLock(t *testing.T) {
	runtime := dbfx.Runtime(t, "Cancel lock ACL")
	agent := dbfx.Agent(t, "Cancel lock ACL", runtime)
	session := createHandlerTestChatSession(t, agent)
	task := dbfx.Task(t, agent, testutil.Cols{"chat_session_id": session, "runtime_id": runtime, "status": "running"})
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var blocker int
	if err = tx.QueryRow(ctx, "SELECT pg_backend_pid() FROM chat_session WHERE id=$1 FOR UPDATE", session).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE chat_participant SET revoked_at=now() WHERE chat_session_id=$1", session); err != nil {
		t.Fatal(err)
	}
	r := cancelTaskByUserRequest(t, testUserID, task)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); testHandler.CancelTaskByUser(w, r) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		if err = testPool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))", blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation did not wait for conversation lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation remained blocked")
	}
	if w.Code != 404 || taskStatus(t, task) != "running" {
		t.Fatalf("revoked cancellation mutated task: status=%s http=%d body=%s", taskStatus(t, task), w.Code, w.Body.String())
	}
}

func TestTeamChat_FilesTicketRangeAndOrigin(t *testing.T) {
	t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
	t.Setenv("LOCAL_UPLOAD_BASE_URL", "")
	store := storage.NewLocalStorageFromEnv()
	h := *testHandler
	h.Storage = store
	id := teamConversation(t, "private_channel")
	peer := dbfx.User(t, "File Peer", "file-peer-"+uuid.NewString()+"@test.invalid")
	dbfx.Member(t, testWorkspaceID, peer, "admin")
	key := testWorkspaceID + "/legacy-" + uuid.NewString() + ".txt"
	link, err := store.Upload(context.Background(), key, []byte("private body"), "text/plain", "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	attID := dbfx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "chat_session_id": id, "uploader_type": "member", "uploader_id": testUserID, "filename": "file.txt", "url": link, "content_type": "text/plain", "size_bytes": 12})
	request := func(user, path string) *http.Request {
		r := teamRequest(t, "GET", path, user, id)
		rc := chi.RouteContext(r.Context())
		rc.URLParams.Add("id", attID)
		return r
	}
	for _, fn := range []http.HandlerFunc{h.GetAttachmentByID, h.GetAttachmentContent, h.DownloadAttachment} {
		testutil.Call(t, fn, request(peer, "/")).Want(404)
	}
	var att AttachmentResponse
	testutil.Call(t, h.GetAttachmentByID, request(testUserID, "/")).Want(200).JSON(&att)
	if strings.Contains(att.URL, "uploads") || !strings.Contains(att.DownloadURL, "chat_sig") {
		t.Fatalf("origin or bearer URL leaked: %+v", att)
	}
	testutil.Call(t, h.DownloadAttachmentWithCapability, request("", att.DownloadURL)).Want(404)
	testutil.Call(t, h.DownloadAttachmentWithCapability, request(peer, att.DownloadURL)).Want(404)
	allowed := request(testUserID, att.DownloadURL)
	allowed.Header.Set("Range", "bytes=0-6")
	response := httptest.NewRecorder()
	h.DownloadAttachmentWithCapability(response, allowed)
	if response.Code != 206 || response.Body.String() != "private" {
		t.Fatalf("range: %d %q", response.Code, response.Body.String())
	}
	testutil.Call(t, h.ServeLocalUpload, request(peer, link)).Want(404)
	testutil.Call(t, h.ServeLocalUpload, request("", link)).Want(404)
	testutil.Call(t, h.ServeLocalUpload, request(peer, strings.Replace(link, "/uploads/", "/uploads/./", 1))).Want(404)
	dbfx.Exec(t, "UPDATE chat_session SET acl_version=acl_version+1 WHERE id=$1", id)
	testutil.Call(t, h.DownloadAttachmentWithCapability, request(testUserID, att.DownloadURL)).Want(404)
	dbfx.Exec(t, "UPDATE chat_participant SET revoked_at=now() WHERE chat_session_id=$1", id)
	testutil.Call(t, h.DownloadAttachment, request(testUserID, "/")).Want(404)
	dbfx.Exec(t, "DELETE FROM attachment WHERE id=$1", attID)
	testutil.Call(t, h.ServeLocalUpload, request(testUserID, link)).Want(404)
	// An unrelated issue object still uses its existing public local path.
	publicURL, err := store.Upload(context.Background(), "issue-file.txt", []byte("issue"), "text/plain", "issue.txt")
	if err != nil {
		t.Fatal(err)
	}
	testutil.Call(t, h.ServeLocalUpload, request("", publicURL)).Want(200)
}

func TestTeamChat_PagingSnapshotAndNoAgentTask(t *testing.T) {
	id := teamConversation(t, "private_channel")
	ctx := context.Background()
	create := func(content string) {
		err := chataccess.Mutate(ctx, testPool, testHandler.Queries, parseUUID(testUserID), parseUUID(testWorkspaceID), parseUUID(id), 1, func(q *db.Queries, a db.GetChatAccessRow) error {
			if err := chataccess.Check(a, chataccess.Send); err != nil {
				return err
			}
			_, err := q.CreateHumanChatMessage(ctx, db.CreateHumanChatMessageParams{ID: parseUUID(uuid.NewString()), ChatSessionID: a.ID, UserID: parseUUID(testUserID), Content: content, ClientMessageID: parseUUID(uuid.NewString())})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		create(fmt.Sprint(i))
	}
	var first struct {
		Messages []db.ChatMessage `json:"messages"`
		Next     string           `json:"next_cursor"`
	}
	testutil.Call(t, testHandler.ListChatMessagesV2, teamRequest(t, "GET", "/?limit=2", testUserID, id)).Want(200).JSON(&first)
	create("later")
	seen := map[int64]bool{}
	for _, m := range first.Messages {
		seen[m.MessageSeq] = true
	}
	next := first.Next
	for next != "" {
		var page struct {
			Messages []db.ChatMessage `json:"messages"`
			Next     string           `json:"next_cursor"`
		}
		testutil.Call(t, testHandler.ListChatMessagesV2, teamRequest(t, "GET", "/?limit=2&before="+next, testUserID, id)).Want(200).JSON(&page)
		for _, m := range page.Messages {
			if seen[m.MessageSeq] || m.MessageSeq > 5 {
				t.Fatalf("duplicate/new snapshot message %d", m.MessageSeq)
			}
			seen[m.MessageSeq] = true
		}
		next = page.Next
	}
	if len(seen) != 5 {
		t.Fatalf("history gap: %v", seen)
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE chat_session_id=$1", id); n != 0 {
		t.Fatalf("human send invoked agent: %d", n)
	}
	testutil.Call(t, testHandler.ListChatMessagesV2, teamRequest(t, "GET", "/?before=bad", testUserID, id)).Want(400)
}

func TestTeamChat_RevocationSerializesContent(t *testing.T) {
	ctx := context.Background()
	id := teamConversation(t, "private_channel")
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	qt := testHandler.Queries.WithTx(tx)
	if _, err = qt.LockChatConversation(ctx, db.LockChatConversationParams{ID: parseUUID(id), WorkspaceID: parseUUID(testWorkspaceID)}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- chataccess.Mutate(ctx, testPool, testHandler.Queries, parseUUID(testUserID), parseUUID(testWorkspaceID), parseUUID(id), 1, func(q *db.Queries, a db.GetChatAccessRow) error {
			_, err := q.CreateHumanChatMessage(ctx, db.CreateHumanChatMessageParams{ID: parseUUID(uuid.NewString()), ChatSessionID: a.ID, UserID: parseUUID(testUserID), Content: "must be refused", ClientMessageID: parseUUID(uuid.NewString())})
			return err
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("writer bypassed conversation lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	_, err = tx.Exec(ctx, "UPDATE chat_participant SET revoked_at=now() WHERE chat_session_id=$1", id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = qt.BumpChatACL(ctx, parseUUID(id)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != chataccess.ErrInvisible {
			t.Fatalf("stale ACL accepted: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer stuck")
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM chat_message WHERE chat_session_id=$1", id); n != 0 {
		t.Fatalf("unauthorized write persisted: %d", n)
	}
}

func TestTeamChat_CapabilitiesSafeUntilIntegration(t *testing.T) {
	r := httptest.NewRecorder()
	testHandler.GetChatCapabilities(r, httptest.NewRequest("GET", "/", nil))
	var cap map[string]any
	if json.Unmarshal(r.Body.Bytes(), &cap) != nil || cap["team_enabled"] != false || cap["protocol"] != float64(2) {
		t.Fatalf("unsafe capabilities: %s", r.Body.String())
	}
}

func TestTeamChat_MembershipCASAndMetadataPreview(t *testing.T) {
	id := teamConversation(t, "private_channel")
	peer := dbfx.User(t, "Invite", "invite-"+uuid.NewString()+"@test.invalid")
	dbfx.Member(t, testWorkspaceID, peer, "member")
	done := make(chan error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			done <- chataccess.Mutate(context.Background(), testPool, testHandler.Queries, parseUUID(testUserID), parseUUID(testWorkspaceID), parseUUID(id), 1, func(q *db.Queries, a db.GetChatAccessRow) error {
				return chataccess.ChangeParticipant(context.Background(), q, a, parseUUID(peer), "participant", false)
			})
		}()
	}
	close(start)
	success, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err == nil {
				success++
			} else if err == chataccess.ErrConflict {
				conflicts++
			} else {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("membership operation stuck")
		}
	}
	if success != 1 || conflicts != 1 || dbfx.Count(t, "SELECT count(*) FROM chat_participant WHERE chat_session_id=$1 AND actor_id=$2", id, peer) != 1 {
		t.Fatalf("CAS success=%d conflicts=%d", success, conflicts)
	}
	patch := func(user, body string) *http.Request {
		r := teamRequest(t, "PATCH", "/", user, id)
		r.Body = io.NopCloser(strings.NewReader(body))
		return r
	}
	// An ordinary participant cannot expose the channel to the workspace.
	testutil.Call(t, testHandler.ChangeChatMetadataV2, patch(peer, `{"expected_revision":2,"kind":"public_channel","preview":true}`)).Want(403)
	testutil.Call(t, testHandler.ChangeChatMetadataV2, patch(testUserID, `{"expected_revision":2,"kind":"public_channel"}`)).Want(403)
	var preview struct {
		Confirmation string `json:"confirmation"`
		Expires      int64  `json:"expires"`
	}
	testutil.Call(t, testHandler.ChangeChatMetadataV2, patch(testUserID, `{"expected_revision":2,"kind":"public_channel","preview":true}`)).Want(200).JSON(&preview)
	body := fmt.Sprintf(`{"expected_revision":2,"kind":"public_channel","confirmation":%q,"expires":%d}`, preview.Confirmation, preview.Expires)
	testutil.Call(t, testHandler.ChangeChatMetadataV2, patch(testUserID, body)).Want(200)
	testutil.Call(t, testHandler.ChangeChatMetadataV2, patch(testUserID, body)).Want(409)
	var s db.ChatSession
	if err := testPool.QueryRow(context.Background(), "SELECT revision FROM chat_session WHERE id=$1", id).Scan(&s.Revision); err != nil || s.Revision != 3 {
		t.Fatalf("revision=%d %v", s.Revision, err)
	}
}

func TestTeamChat_PrivateTextExcludedFromSearchSnapshot(t *testing.T) {
	id := teamConversation(t, "private_channel")
	sentinel := "private-only-" + uuid.NewString()
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": id, "role": "user", "content": sentinel, "actor_type": "member", "actor_id": testUserID})
	peer := dbfx.User(t, "Search peer", "search-peer-"+uuid.NewString()+"@test.invalid")
	dbfx.Member(t, testWorkspaceID, peer, "admin")
	w := httptest.NewRecorder()
	testHandler.GetSearchIndexSnapshot(w, teamRequest(t, "GET", "/", peer, id))
	if w.Code != 200 || strings.Contains(w.Body.String(), sentinel) || strings.Contains(w.Body.String(), id) {
		t.Fatalf("private chat leaked or snapshot not tested: %d %s", w.Code, w.Body.String())
	}
}

type revokeOnOpenStorage struct {
	storage.Storage
	revoke func()
}

func (s revokeOnOpenStorage) GetReader(ctx context.Context, key string) (io.ReadCloser, error) {
	reader, err := s.Storage.GetReader(ctx, key)
	if err == nil {
		s.revoke()
	}
	return reader, err
}

func TestTeamChat_FileRevokeWhileStorageOpens(t *testing.T) {
	t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
	t.Setenv("LOCAL_UPLOAD_BASE_URL", "")
	store := storage.NewLocalStorageFromEnv()
	id := teamConversation(t, "private_channel")
	link, err := store.Upload(context.Background(), "private-chat/"+uuid.NewString()+".txt", []byte("must never transfer"), "text/plain", "secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	att := dbfx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "chat_session_id": id, "uploader_type": "member", "uploader_id": testUserID, "filename": "secret.txt", "url": link, "content_type": "text/plain", "size_bytes": 19})
	h := *testHandler
	h.Storage = revokeOnOpenStorage{Storage: store, revoke: func() {
		dbfx.Exec(t, "UPDATE chat_participant SET revoked_at=now() WHERE chat_session_id=$1", id)
		dbfx.Exec(t, "UPDATE chat_session SET acl_version=acl_version+1 WHERE id=$1", id)
	}}
	for _, tc := range []struct {
		name   string
		fn     http.HandlerFunc
		origin bool
	}{{"download", h.DownloadAttachment, false}, {"ticket", h.DownloadAttachmentWithCapability, false}, {"preview", h.GetAttachmentContent, false}, {"origin", h.ServeLocalUpload, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dbfx.Exec(t, "UPDATE chat_participant SET revoked_at=NULL WHERE chat_session_id=$1", id)
			r := teamRequest(t, "GET", "/", testUserID, id)
			chi.RouteContext(r.Context()).URLParams.Add("id", att)
			var metadata AttachmentResponse
			testutil.Call(t, h.GetAttachmentByID, r).Want(200).JSON(&metadata)
			r.URL, err = url.Parse(metadata.DownloadURL)
			if err != nil {
				t.Fatal(err)
			}
			if tc.origin {
				r.URL, _ = url.Parse(link)
			}
			w := httptest.NewRecorder()
			tc.fn(w, r)
			if w.Code != 404 || strings.Contains(w.Body.String(), "must never transfer") {
				t.Fatalf("storage-open revoke transferred data: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

type revokeAfterWrite struct {
	*httptest.ResponseRecorder
	revoke func()
}

func (w *revokeAfterWrite) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if w.revoke != nil {
		w.revoke()
		w.revoke = nil
	}
	return n, err
}

func TestTeamChat_FileRevokeStopsNextStreamChunk(t *testing.T) {
	t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
	t.Setenv("LOCAL_UPLOAD_BASE_URL", "")
	store := storage.NewLocalStorageFromEnv()
	id := teamConversation(t, "private_channel")
	body := strings.Repeat("private", 10000)
	link, err := store.Upload(context.Background(), "private-chat/"+uuid.NewString()+".txt", []byte(body), "text/plain", "secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	att := dbfx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "chat_session_id": id, "uploader_type": "member", "uploader_id": testUserID, "filename": "secret.txt", "url": link, "content_type": "text/plain", "size_bytes": len(body)})
	h := *testHandler
	h.Storage = store
	r := teamRequest(t, "GET", "/", testUserID, id)
	chi.RouteContext(r.Context()).URLParams.Add("id", att)
	w := &revokeAfterWrite{ResponseRecorder: httptest.NewRecorder(), revoke: func() {
		dbfx.Exec(t, "UPDATE chat_participant SET revoked_at=now() WHERE chat_session_id=$1", id)
		dbfx.Exec(t, "UPDATE chat_session SET acl_version=acl_version+1 WHERE id=$1", id)
	}}
	h.DownloadAttachment(w, r)
	if n := w.Body.Len(); n == 0 || n >= len(body) {
		t.Fatalf("revoke must stop subsequent chunks, transferred=%d total=%d", n, len(body))
	}
}
