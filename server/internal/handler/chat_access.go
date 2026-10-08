package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/chataccess"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func chatCancellationAuth(r *http.Request, workspace, conversation pgtype.UUID) func(context.Context, *db.Queries) error {
	if !conversation.Valid {
		return nil
	}
	return func(ctx context.Context, q *db.Queries) error {
		_, err := chataccess.Read(ctx, q, parseUUID(requestUserID(r)), workspace, conversation)
		return err
	}
}

func chatAccessError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chataccess.ErrInvisible):
		writeError(w, http.StatusNotFound, "conversation not found")
	case errors.Is(err, chataccess.ErrForbidden):
		writeError(w, http.StatusForbidden, "action not permitted")
	case errors.Is(err, chataccess.ErrConflict):
		writeError(w, http.StatusConflict, "conversation revision changed")
	default:
		writeError(w, http.StatusInternalServerError, "failed to authorize conversation")
	}
}

// Task credentials are restricted to their own live invocation. A2A visibility
// of an agent is never a grant to the human's other private conversations.
func (h *Handler) authorizeChatRequest(w http.ResponseWriter, r *http.Request, workspace, session pgtype.UUID, legacy ...bool) (db.GetChatAccessRow, bool) {
	if r.Header.Get("X-User-ID") == "" {
		writeError(w, http.StatusNotFound, "conversation not found")
		return db.GetChatAccessRow{}, false
	}
	user, ok := parseUUIDOrBadRequest(w, r.Header.Get("X-User-ID"), "user id")
	if !ok {
		return db.GetChatAccessRow{}, false
	}
	if r.Header.Get("X-Actor-Source") == "task_token" {
		taskID, ok := parseUUIDOrBadRequest(w, r.Header.Get("X-Task-ID"), "task id")
		if !ok {
			return db.GetChatAccessRow{}, false
		}
		task, err := h.Queries.GetAgentTask(r.Context(), taskID)
		if err != nil || task.ChatSessionID != session || uuidToString(task.AgentID) != r.Header.Get("X-Agent-ID") || uuidToString(workspace) != r.Header.Get("X-Workspace-ID") || isTerminalTaskStatus(task.Status) {
			chatAccessError(w, chataccess.ErrInvisible)
			return db.GetChatAccessRow{}, false
		}
	}
	read := chataccess.Read
	if len(legacy) > 0 && legacy[0] {
		read = chataccess.ReadLegacy
	}
	a, err := read(r.Context(), h.Queries, user, workspace, session)
	if err != nil {
		chatAccessError(w, err)
		return a, false
	}
	return a, true
}

func (h *Handler) authorizeChatAttachment(w http.ResponseWriter, r *http.Request, att db.Attachment) bool {
	session := att.ChatSessionID
	if att.ChatMessageID.Valid {
		message, err := h.Queries.GetChatMessageByID(r.Context(), att.ChatMessageID)
		if err != nil || message.DeletedAt.Valid || (session.Valid && session != message.ChatSessionID) {
			writeError(w, http.StatusNotFound, "attachment not found")
			return false
		}
		session = message.ChatSessionID
	}
	if !session.Valid {
		return true
	}
	if _, ok := h.authorizeChatRequest(w, r, att.WorkspaceID, session); !ok {
		return false
	}
	if r.Header.Get("X-Actor-Source") == "task_token" && uuidToString(att.TaskID) != r.Header.Get("X-Task-ID") {
		writeError(w, http.StatusNotFound, "attachment not found")
		return false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	return true
}

// Storage reads can involve network I/O. Do not transfer bytes/metadata using
// an attachment or capability authorized before that I/O completed.
func (h *Handler) recheckChatAttachment(w http.ResponseWriter, r *http.Request, att db.Attachment) bool {
	if !att.ChatSessionID.Valid && !att.ChatMessageID.Valid {
		return true
	}
	current, err := h.Queries.GetAttachmentByIDOnly(r.Context(), att.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "attachment not found")
		return false
	}
	if r.URL.Query().Get("chat_sig") != "" {
		return h.redeemChatAttachment(w, r, current)
	}
	return h.authorizeChatAttachment(w, r, current)
}

// One batch lookup protects list projections, including tasks whose historical
// FK was cleared by deleting a conversation. No per-issue-task ACL query loop.
func (h *Handler) readableChatTaskIDs(r *http.Request, workspace pgtype.UUID, ids []pgtype.UUID) (map[pgtype.UUID]bool, error) {
	protected, err := h.Queries.ListChatProtectedTasks(r.Context(), ids)
	if err != nil {
		return nil, err
	}
	allowed := make(map[pgtype.UUID]bool, len(protected))
	for _, p := range protected {
		allowed[p.TaskID] = false
		if !p.ChatSessionID.Valid || (r.Header.Get("X-Actor-Source") == "task_token" && uuidToString(p.TaskID) != r.Header.Get("X-Task-ID")) {
			continue
		}
		if _, err := chataccess.Read(r.Context(), h.Queries, parseUUID(requestUserID(r)), workspace, p.ChatSessionID); err == nil {
			allowed[p.TaskID] = true
		}
	}
	return allowed, nil
}

// Legacy mutations share the conversation lock with v2 membership changes.
func (h *Handler) lockChatForRequest(w http.ResponseWriter, r *http.Request, s db.ChatSession) (*db.Queries, pgx.Tx, bool) {
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		chatAccessError(w, err)
		return nil, nil, false
	}
	q := h.Queries.WithTx(tx)
	if _, err = q.LockChatConversation(r.Context(), db.LockChatConversationParams{ID: s.ID, WorkspaceID: s.WorkspaceID}); err != nil {
		tx.Rollback(r.Context())
		chatAccessError(w, chataccess.ErrInvisible)
		return nil, nil, false
	}
	scoped := *h
	scoped.Queries = q
	if _, ok := scoped.authorizeChatRequest(w, r, s.WorkspaceID, s.ID, true); !ok {
		tx.Rollback(r.Context())
		return nil, nil, false
	}
	return q, tx, true
}
