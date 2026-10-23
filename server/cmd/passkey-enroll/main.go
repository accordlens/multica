// passkey-enroll prints a one-time link that lets a person register a
// passkey on a passkey-only deployment (ACCO-55). It talks to the database
// directly, so only someone with access to the host can run it:
//
//	docker compose exec backend ./passkey-enroll --email owner@example.com
//
// Use it to bootstrap the first owner on a fresh instance, to give an
// existing account (one created before passkey-only) its first passkey, and
// to recover a person who lost every passkey. Workspace admins invite new
// members from the app instead.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/passkey"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "passkey-enroll:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("passkey-enroll", flag.ContinueOnError)
	email := fs.String("email", "", "email of the account to enroll (created if missing)")
	name := fs.String("name", "", "display name for a new account (default: part of the email before @)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	addr := strings.ToLower(strings.TrimSpace(*email))
	if !strings.Contains(addr, "@") {
		return errors.New("--email is required")
	}

	appURL := strings.TrimSpace(os.Getenv("MULTICA_APP_URL"))
	if appURL == "" {
		appURL = strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	}
	cfg, err := passkey.ConfigFromAppURL(appURL, os.Getenv("MULTICA_PASSKEY_RP_ID"))
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	link, expiresAt, err := enroll(ctx, db.New(pool), cfg.Origin, addr, strings.TrimSpace(*name))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Enrollment link for %s (single use, expires %s):\n\n%s\n\n", addr, expiresAt.UTC().Format(time.RFC3339), link)
	fmt.Fprintln(out, "Anyone who opens this link can add a passkey to this account. Hand it over privately.")
	return nil
}

func enroll(ctx context.Context, q *db.Queries, origin, email, name string) (string, time.Time, error) {
	user, err := q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		if name == "" {
			name = email[:strings.Index(email, "@")]
		}
		user, err = q.CreateUser(ctx, db.CreateUserParams{Name: name, Email: email})
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("load or create account: %w", err)
	}
	token, expiresAt, err := passkey.IssueEnrollmentToken(ctx, q, user.ID, pgtype.UUID{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("issue enrollment token: %w", err)
	}
	return passkey.EnrollmentURL(origin, token), expiresAt, nil
}
