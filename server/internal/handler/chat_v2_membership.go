package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/chataccess"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This endpoint establishes the server policy used by the C3 management UI.
func (h *Handler) ChangeChatParticipantV2(w http.ResponseWriter, r *http.Request) {
	s, ok := h.loadChatConversationV2(w, r)
	if !ok {
		return
	}
	if r.Header.Get("X-Actor-Source") == "task_token" {
		writeError(w, http.StatusForbidden, "human membership action required")
		return
	}
	target, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "userId"), "participant user id")
	if !ok {
		return
	}
	var req struct {
		ExpectedRevision int64  `json:"expected_revision"`
		Role             string `json:"role"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil || req.ExpectedRevision < 1 {
		writeError(w, http.StatusBadRequest, "expected_revision required")
		return
	}
	if req.Role == "" {
		req.Role = "participant"
	}
	revoke := r.Method == http.MethodDelete
	err := chataccess.Mutate(r.Context(), h.TxStarter, h.Queries, parseUUID(requestUserID(r)), s.WorkspaceID, s.ID, req.ExpectedRevision, func(q *db.Queries, a db.GetChatAccessRow) error {
		return chataccess.ChangeParticipant(r.Context(), q, a, target, req.Role, revoke)
	})
	if err != nil {
		chatAccessError(w, err)
		return
	}
	if revoke {
		h.publishChatAccessRevoked(uuidToString(target), uuidToString(s.WorkspaceID), uuidToString(s.ID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": s.Revision + 1, "acl_version": s.AclVersion + 1})
}

// A revocation carries identifiers only, never the name or private transcript.
func (h *Handler) publishChatAccessRevoked(userID, workspaceID, sessionID string) {
	h.Bus.Publish(events.Event{Type: "chat:access_revoked", WorkspaceID: workspaceID, Payload: map[string]any{"recipient_id": userID, "chat_session_id": sessionID, "workspace_id": workspaceID}})
}
