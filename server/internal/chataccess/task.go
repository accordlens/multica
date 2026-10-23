package chataccess

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type protectedTaskQuerier interface {
	GetChatProtectedTask(context.Context, pgtype.UUID) (db.ChatProtectedTask, error)
}

// TaskConversation never treats a deleted chat task as an ordinary public run.
func TaskConversation(ctx context.Context, q protectedTaskQuerier, task db.AgentTaskQueue) (pgtype.UUID, error) {
	if task.ChatSessionID.Valid {
		return task.ChatSessionID, nil
	}
	p, err := q.GetChatProtectedTask(ctx, task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	if !p.ChatSessionID.Valid {
		return pgtype.UUID{}, ErrInvisible
	}
	return p.ChatSessionID, nil
}
