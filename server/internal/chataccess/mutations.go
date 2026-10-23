package chataccess

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type TxStarter interface {
	Begin(context.Context) (pgx.Tx, error)
}

// Mutate is the mandatory entry point for v2 membership/content changes.
// The session lock precedes ACL/revision reads, sequence allocation and writes.
// The callback must perform database work only; delivery happens after commit.
func Mutate(ctx context.Context, starter TxStarter, q *db.Queries, user, workspace, session pgtype.UUID, revision int64, fn func(*db.Queries, db.GetChatAccessRow) error) error {
	tx, err := starter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qt := q.WithTx(tx)
	if _, err = qt.LockChatConversation(ctx, db.LockChatConversationParams{ID: session, WorkspaceID: workspace}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvisible
		}
		return err
	}
	a, err := Read(ctx, qt, user, workspace, session)
	if err != nil {
		return err
	}
	if revision != a.Revision {
		return ErrConflict
	}
	if err = fn(qt, a); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func ChangeParticipant(ctx context.Context, q *db.Queries, a db.GetChatAccessRow, target pgtype.UUID, role string, revoke bool) error {
	action := Invite
	if revoke {
		action = Remove
	}
	if err := Check(a, action); err != nil {
		return err
	}
	if role != "participant" && role != "manager" {
		return ErrForbidden
	}
	if role == "manager" && a.ParticipantRole != "manager" && a.WorkspaceRole != "owner" && a.WorkspaceRole != "admin" {
		return ErrForbidden
	}
	if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: target, WorkspaceID: a.WorkspaceID}); err != nil {
		return ErrInvisible
	}
	if u, err := q.GetUser(ctx, target); err != nil || u.DeactivatedAt.Valid {
		return ErrInvisible
	}
	revoked := pgtype.Timestamptz{}
	if revoke {
		revoked = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	if err := q.ChangeChatParticipant(ctx, db.ChangeChatParticipantParams{ChatSessionID: a.ID, UserID: target, Role: role, RevokedAt: revoked}); err != nil {
		return err
	}
	_, err := q.BumpChatACL(ctx, a.ID)
	return err
}
