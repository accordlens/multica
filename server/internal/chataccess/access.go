// Package chataccess is the conversation boundary shared by HTTP, files and WS.
package chataccess

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrInvisible = errors.New("conversation not found")
var ErrForbidden = errors.New("action not permitted")
var ErrConflict = errors.New("conversation revision changed")

type Querier interface {
	GetChatAccess(context.Context, db.GetChatAccessParams) (db.GetChatAccessRow, error)
	GetAgent(context.Context, pgtype.UUID) (db.Agent, error)
	ListAgentInvocationTargets(context.Context, pgtype.UUID) ([]db.AgentInvocationTarget, error)
}

// Read always consults the database. A workspace admin cannot bypass private
// participants; legacy agent sessions also retain the existing agent view gate.
func Read(ctx context.Context, q Querier, userID, workspaceID, sessionID pgtype.UUID) (db.GetChatAccessRow, error) {
	return read(ctx, q, userID, workspaceID, sessionID, false)
}

// ReadLegacy preserves the installed agent-chat API's 403 for a creator who
// lost agent visibility. Missing membership and team conversations stay hidden.
func ReadLegacy(ctx context.Context, q Querier, userID, workspaceID, sessionID pgtype.UUID) (db.GetChatAccessRow, error) {
	return read(ctx, q, userID, workspaceID, sessionID, true)
}

func read(ctx context.Context, q Querier, userID, workspaceID, sessionID pgtype.UUID, legacy bool) (db.GetChatAccessRow, error) {
	a, err := q.GetChatAccess(ctx, db.GetChatAccessParams{UserID: userID, WorkspaceID: workspaceID, ChatSessionID: sessionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrInvisible
	}
	if err != nil {
		return a, err
	}
	if auth.IsTemporarilyDisabledUser(util.UUIDToString(userID), a.Email) {
		return a, ErrInvisible
	}
	if a.Kind != "agent_dm" {
		return a, nil
	}
	agent, err := q.GetAgent(ctx, a.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrInvisible
	}
	if err != nil {
		return a, err
	}
	if agent.WorkspaceID != workspaceID {
		return a, ErrInvisible
	}
	if agent.OwnerID == userID || a.WorkspaceRole == "owner" || a.WorkspaceRole == "admin" {
		return a, nil
	}
	denied := ErrInvisible
	if legacy {
		denied = ErrForbidden
	}
	if agent.PermissionMode != "public_to" {
		return a, denied
	}
	targets, err := q.ListAgentInvocationTargets(ctx, agent.ID)
	if err != nil {
		return a, err
	}
	for _, t := range targets {
		if t.TargetType == "workspace" || (t.TargetType == "member" && t.TargetID == userID) {
			return a, nil
		}
	}
	return a, denied
}

type Action string

const (
	Send        Action = "send"
	Invite      Action = "invite"
	Remove      Action = "remove"
	Rename      Action = "rename"
	Topic       Action = "topic"
	Archive     Action = "archive"
	Moderate    Action = "moderate"
	MakePublic  Action = "make_public"
	MakePrivate Action = "make_private"
)

// Check acts only on a resource already authorized by Read.
func Check(a db.GetChatAccessRow, action Action) error {
	participant := a.ParticipantRole != ""
	manager := a.ParticipantRole == "manager"
	admin := a.WorkspaceRole == "owner" || a.WorkspaceRole == "admin"
	channel := a.Kind == "public_channel" || a.Kind == "private_channel"
	if a.Kind == "agent_dm" {
		return ErrForbidden
	} // legacy has its own invoke/send path
	switch action {
	case Send:
		if participant && a.Status == "active" {
			return nil
		}
	case Invite:
		if channel && participant {
			return nil
		}
	case Rename:
		if a.Kind == "group_dm" && participant {
			return nil
		}
		fallthrough
	case Remove, Archive:
		if channel && (!a.IsGeneral || action == Rename) && (manager || admin) && (a.Kind == "public_channel" || participant) {
			return nil
		}
	case Topic:
		if participant {
			return nil
		}
	case Moderate:
		if participant && admin && (channel || a.Kind == "group_dm") {
			return nil
		}
	case MakePublic:
		if a.Kind == "private_channel" && participant && a.WorkspaceRole == "owner" {
			return nil
		}
	case MakePrivate:
		if a.Kind == "public_channel" && !a.IsGeneral && (manager || admin) {
			return nil
		}
	}
	return ErrForbidden
}
