package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestEnrollCreatesAccountOnceAndIssuesFreshLinks(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)

	email := "owner-" + uuid.NewString()[:8] + "@multica.test"
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM passkey_enrollment_token WHERE user_id IN (SELECT id FROM "user" WHERE email = $1)`, email)
		pool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)
	})

	first, _, err := enroll(ctx, q, "https://multica.test.ts.net", email, "")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := enroll(ctx, q, "https://multica.test.ts.net", email, "")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "https://multica.test.ts.net/login?enroll=") {
		t.Fatalf("links %q and %q must be distinct enrollment URLs", first, second)
	}
	var users, tokens int
	pool.QueryRow(ctx, `SELECT count(*) FROM "user" WHERE email = $1`, email).Scan(&users)
	pool.QueryRow(ctx, `SELECT count(*) FROM passkey_enrollment_token t JOIN "user" u ON u.id = t.user_id WHERE u.email = $1`, email).Scan(&tokens)
	if users != 1 || tokens != 2 {
		t.Fatalf("users = %d, tokens = %d; want 1 account and 2 links", users, tokens)
	}
	user, _ := q.GetUserByEmail(ctx, email)
	if user.Name != strings.Split(email, "@")[0] {
		t.Fatalf("default name = %q", user.Name)
	}
}

func TestRunRefusesNonHTTPSOrigin(t *testing.T) {
	t.Setenv("MULTICA_APP_URL", "http://192.168.1.10:3000")
	if err := run(context.Background(), []string{"--email", "a@b.c"}, os.Stdout); err == nil {
		t.Fatal("want error for a non-https app origin")
	}
}
