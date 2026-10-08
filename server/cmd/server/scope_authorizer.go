package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/chataccess"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// scopeAuthQuerier is the narrow subset of db.Queries used by the scope
// authorizer. Declared as an interface so the authorizer can be unit tested
// with an in-memory fake (no DB required).
type scopeAuthQuerier interface {
	chataccess.Querier
	GetAgentTask(ctx context.Context, id pgtype.UUID) (db.AgentTaskQueue, error)
	GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error)
	GetChatSession(ctx context.Context, id pgtype.UUID) (db.ChatSession, error)
	GetChatProtectedTask(context.Context, pgtype.UUID) (db.ChatProtectedTask, error)
}

// dbScopeAuthorizer implements realtime.ScopeAuthorizer for the per-task and
// per-chat scopes (workspace/user scopes are validated by the hub itself
// against the connection identity). It returns true only when the requested
// resource exists, belongs to the caller's workspace, and — for chat
// resources — was created by the caller (mirroring the HTTP creator-only
// access model).
type dbScopeAuthorizer struct{ q scopeAuthQuerier }

func newScopeAuthorizer(q scopeAuthQuerier) *dbScopeAuthorizer { return &dbScopeAuthorizer{q: q} }

// scopeLookupErr converts a scope-resource query error into an authorizer
// result. A missing resource (pgx.ErrNoRows) is a legitimate denial — the
// HTTP layer treats not-found as 404 rather than 403, so the realtime layer
// reports it as a plain "forbidden" refusal. Any other error (pool
// exhaustion, a cancelled context, a network blip) is a transient lookup
// failure and must propagate so handleSubscribe reports "lookup_failed"
// instead of masking a database outage as a wave of permission denials.
func scopeLookupErr(err error) (bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func (a *dbScopeAuthorizer) AuthorizeScope(ctx context.Context, userID, workspaceID, scopeType, scopeID string) (bool, error) {
	if workspaceID == "" || scopeID == "" {
		return false, nil
	}
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return false, nil
	}
	idUUID, err := util.ParseUUID(scopeID)
	if err != nil {
		return false, nil
	}
	switch scopeType {
	case realtime.ScopeTask:
		task, err := a.q.GetAgentTask(ctx, idUUID)
		if err != nil {
			return scopeLookupErr(err)
		}
		conversation, err := chataccess.TaskConversation(ctx, a.q, task)
		if err != nil {
			if errors.Is(err, chataccess.ErrInvisible) {
				return false, nil
			}
			return false, err
		}
		if conversation.Valid {
			_, err := chataccess.Read(ctx, a.q, mustParsedUser(userID), wsUUID, conversation)
			if errors.Is(err, chataccess.ErrInvisible) {
				return false, nil
			}
			return err == nil, err
		}
		// Issue tasks: visible to any workspace member.
		if task.IssueID.Valid {
			issue, err := a.q.GetIssue(ctx, task.IssueID)
			if err != nil {
				return scopeLookupErr(err)
			}
			return issue.WorkspaceID == wsUUID, nil
		}
		return false, nil
	case realtime.ScopeChat:
		uid, err := util.ParseUUID(userID)
		if err != nil {
			return false, nil
		}
		_, err = chataccess.Read(ctx, a.q, uid, wsUUID, idUUID)
		if errors.Is(err, chataccess.ErrInvisible) {
			return false, nil
		}
		return err == nil, err
	default:
		return false, nil
	}
}

func mustParsedUser(userID string) pgtype.UUID {
	id, _ := util.ParseUUID(userID)
	return id
}

// AuthorizeDelivery also covers old clients without subscribe frames and frames
// retained by a relay before a membership change. No payload is trusted as ACL.
func (a *dbScopeAuthorizer) AuthorizeDelivery(ctx context.Context, userID, workspaceID string, frame []byte) (bool, error) {
	var f struct {
		Type           string `json:"type"`
		ConversationID string `json:"conversation_id"`
		TaskID         string `json:"task_id"`
		WorkspaceID    string `json:"workspace_id"`
		RecipientID    string `json:"recipient_id"`
	}
	if err := json.Unmarshal(frame, &f); err != nil {
		return false, err
	}
	if f.Type == "chat:access_revoked" || f.Type == "chat:session_deleted" {
		return f.RecipientID != "" && f.RecipientID == userID, nil
	}
	if f.ConversationID != "" {
		if f.TaskID != "" {
			id, err := util.ParseUUID(f.TaskID)
			if err != nil {
				return false, nil
			}
			task, err := a.q.GetAgentTask(ctx, id)
			if err != nil {
				return scopeLookupErr(err)
			}
			conversation, err := chataccess.TaskConversation(ctx, a.q, task)
			if err != nil || util.UUIDToString(conversation) != f.ConversationID {
				return false, nil
			}
		}
		return a.AuthorizeScope(ctx, userID, f.WorkspaceID, realtime.ScopeChat, f.ConversationID)
	}
	if strings.HasPrefix(f.Type, "chat:") {
		return false, nil
	}
	if strings.HasPrefix(f.Type, "task:") {
		if f.TaskID == "" {
			return false, nil
		}
		return a.AuthorizeScope(ctx, userID, workspaceID, realtime.ScopeTask, f.TaskID)
	}
	return true, nil
}
