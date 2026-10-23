package handler

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ChatConversationV2Response struct {
	ID             string `json:"id"`
	WorkspaceID    string `json:"workspace_id"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	Topic          string `json:"topic"`
	Description    string `json:"description"`
	Revision       int64  `json:"revision"`
	ACLVersion     int64  `json:"acl_version"`
	LastMessageSeq int64  `json:"last_message_seq"`
}

func (h *Handler) loadChatConversationV2(w http.ResponseWriter, r *http.Request) (db.ChatSession, bool) {
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "conversationId"), "conversation id")
	if !ok {
		return db.ChatSession{}, false
	}
	workspace, ok := parseUUIDOrBadRequest(w, ctxWorkspaceID(r.Context()), "workspace id")
	if !ok {
		return db.ChatSession{}, false
	}
	if _, ok := h.authorizeChatRequest(w, r, workspace, id); !ok {
		return db.ChatSession{}, false
	}
	s, err := h.Queries.GetChatConversation(r.Context(), db.GetChatConversationParams{ID: id, WorkspaceID: workspace})
	if err != nil {
		writeError(w, http.StatusNotFound, "conversation not found")
		return s, false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	return s, true
}

func (h *Handler) GetChatConversationV2(w http.ResponseWriter, r *http.Request) {
	s, ok := h.loadChatConversationV2(w, r)
	if !ok {
		return
	}
	if _, ok := h.authorizeChatRequest(w, r, s.WorkspaceID, s.ID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, ChatConversationV2Response{ID: uuidToString(s.ID), WorkspaceID: uuidToString(s.WorkspaceID), Kind: s.Kind, Name: s.Name, Topic: s.Topic, Description: s.Description, Revision: s.Revision, ACLVersion: s.AclVersion, LastMessageSeq: s.LastMessageSeq})
}

type chatSequenceCursor struct {
	Conversation string `json:"conversation"`
	Root         string `json:"root"`
	Head         int64  `json:"head"`
	Before       int64  `json:"before"`
}

type ChatMessageV2Response struct {
	ID            string  `json:"id"`
	ChatSessionID string  `json:"chat_session_id"`
	ActorType     string  `json:"actor_type"`
	ActorID       string  `json:"actor_id"`
	Content       string  `json:"content"`
	MessageSeq    int64   `json:"message_seq"`
	Revision      int64   `json:"revision"`
	RootMessageID *string `json:"root_message_id"`
	Deleted       bool    `json:"deleted"`
	CreatedAt     string  `json:"created_at"`
}

func (h *Handler) ListChatMessagesV2(w http.ResponseWriter, r *http.Request) {
	s, ok := h.loadChatConversationV2(w, r)
	if !ok {
		return
	}
	root := pgtype.UUID{}
	rootString := r.URL.Query().Get("root")
	if rootString != "" {
		root, ok = parseUUIDOrBadRequest(w, rootString, "root")
		if !ok {
			return
		}
		m, err := h.Queries.GetChatMessageByID(r.Context(), root)
		if err != nil || m.ChatSessionID != s.ID || m.RootMessageID.Valid {
			writeError(w, http.StatusNotFound, "thread not found")
			return
		}
	}
	cursor := chatSequenceCursor{Conversation: uuidToString(s.ID), Root: rootString, Head: s.LastMessageSeq, Before: math.MaxInt64}
	if raw := r.URL.Query().Get("before"); raw != "" {
		data, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Conversation != uuidToString(s.ID) || cursor.Root != rootString || cursor.Head < 0 || cursor.Head > s.LastMessageSeq || cursor.Before < 1 {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	var messages []db.ChatMessage
	var err error
	if root.Valid {
		messages, err = h.Queries.ListChatThreadMessagesBySequence(r.Context(), db.ListChatThreadMessagesBySequenceParams{ChatSessionID: s.ID, RootMessageID: root, SnapshotHead: cursor.Head, BeforeSeq: cursor.Before, PageLimit: int32(limit + 1)})
	} else {
		messages, err = h.Queries.ListChatMessagesBySequence(r.Context(), db.ListChatMessagesBySequenceParams{ChatSessionID: s.ID, SnapshotHead: cursor.Head, BeforeSeq: cursor.Before, PageLimit: int32(limit + 1)})
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read messages")
		return
	}
	next := ""
	if len(messages) > limit {
		messages = messages[:limit]
		cursor.Before = messages[len(messages)-1].MessageSeq
		data, _ := json.Marshal(cursor)
		next = base64.RawURLEncoding.EncodeToString(data)
	}
	// A tombstone never returns former content or internal task/channel metadata.
	response := make([]ChatMessageV2Response, 0, len(messages))
	for i := range messages {
		if messages[i].DeletedAt.Valid {
			messages[i].Content = ""
			messages[i].QuickActions = nil
		}
		m := messages[i]
		response = append(response, ChatMessageV2Response{ID: uuidToString(m.ID), ChatSessionID: uuidToString(m.ChatSessionID), ActorType: m.ActorType.String, ActorID: uuidToString(m.ActorID), Content: m.Content, MessageSeq: m.MessageSeq, Revision: m.Revision, RootMessageID: uuidToPtr(m.RootMessageID), Deleted: m.DeletedAt.Valid, CreatedAt: timestampToString(m.CreatedAt)})
	}
	if _, ok := h.authorizeChatRequest(w, r, s.WorkspaceID, s.ID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": response, "next_cursor": next, "snapshot_head": cursor.Head})
}
