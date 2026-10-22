package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func seedTeamChatCleanupState(t *testing.T, sessionID string) {
	t.Helper()
	root := dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "cleanup root"})
	dbfx.InsertNoID(t, "chat_read_state", testutil.Cols{"chat_session_id": sessionID, "user_id": testUserID}, "chat_session_id=$1", sessionID)
	dbfx.InsertNoID(t, "chat_thread_state", testutil.Cols{"chat_session_id": sessionID, "user_id": testUserID, "root_message_id": root}, "chat_session_id=$1", sessionID)
	dbfx.InsertNoID(t, "chat_draft", testutil.Cols{"chat_session_id": sessionID, "user_id": testUserID, "content": "unsent"}, "chat_session_id=$1", sessionID)
	dbfx.Insert(t, "chat_event", testutil.Cols{"chat_session_id": sessionID, "event_seq": 1, "acl_version": 1, "event_type": "message", "actor_type": "member", "actor_id": testUserID, "payload": "{}"})
}

func assertTeamChatStatePruned(t *testing.T, sessionID string) {
	t.Helper()
	for _, table := range []string{"chat_participant", "chat_read_state", "chat_thread_state", "chat_draft", "chat_event"} {
		if n := dbfx.Count(t, "SELECT count(*) FROM "+table+" WHERE chat_session_id=$1", sessionID); n != 0 {
			t.Errorf("%s left %d orphan rows after conversation deletion", table, n)
		}
	}
}
