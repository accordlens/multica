package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDatabaseConfig(t *testing.T) {
	for _, tc := range []struct{ name, raw, expected string }{
		{"implicit", "", ""},
		{"production", "postgres://localhost/multica", "multica"},
		{"remote", "postgres://example.com/multica_test_kbbench", "multica_test_kbbench"},
		{"mismatch", "postgres://localhost/multica_other_kbbench", "multica_test_kbbench"},
		{"non-benchmark", "postgres://localhost/multica_feature", "multica_feature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := databaseConfig(tc.raw, tc.expected); err == nil {
				t.Fatal("unsafe database target accepted")
			}
		})
	}
	if _, err := databaseConfig("postgres://127.0.0.1/multica_test_kbbench?sslmode=disable", "multica_test_kbbench"); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitBuildIdentity(t *testing.T) {
	previous := commit
	t.Cleanup(func() { commit = previous })
	commit = strings.Repeat("a", 40)
	identity := version()
	if identity["code_sha"] != commit || identity["dirty"] != false {
		t.Fatalf("explicit clean build stamp lost: %v", identity)
	}
}

func TestDeletedTextWithALiveCopyIsNotAttributedToTheTombstone(t *testing.T) {
	documents := []document{{Comments: []source{{Text: "same fact", Deleted: true}, {Text: "Current quote: same fact"}}}}
	if !hasLiveCopy(documents, "same fact") {
		t.Fatal("live duplicate would be falsely attributed to a deleted source")
	}
	if hasLiveCopy(documents, "only deleted") {
		t.Fatal("unrelated live source was treated as a duplicate")
	}
}

func TestPopulatedDatabaseAndCleanupGuards(t *testing.T) {
	if os.Getenv("KBBENCH_RUN_INTEGRATION") != "1" {
		t.Skip("requires an explicitly isolated, migrated kbbench database")
	}
	cfg, err := databaseConfig(os.Getenv("KBBENCH_DATABASE_URL"), os.Getenv("KBBENCH_DATABASE_NAME"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	pool, err := pgxpool.New(ctx, cfg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fx := testutil.New(pool, "", "")
	userID := fx.User(t, "Unrelated guard fixture", uuid.NewString()+"@guard.invalid")
	in := input{Synthetic: true, Plans: []plan{{ID: "mini"}}}
	if _, err := setup(ctx, conn, in, filepath.Join(t.TempDir(), "fixture.json")); err == nil || !strings.Contains(err.Error(), "contains application data") {
		t.Fatalf("populated database accepted: %v", err)
	}
	for _, f := range []fixture{
		{Kind: "multica-kbbench-fixture-v1", Database: "other_database"},
		{Kind: "multica-kbbench-fixture-v1", Database: cfg.Database, Users: []userRecord{{ID: userID, Email: "forged@kbbench.invalid"}}},
	} {
		if err := cleanup(ctx, conn, f); err == nil {
			t.Fatal("unsafe cleanup manifest accepted")
		}
	}
	if got := fx.Count(t, `SELECT count(*) FROM "user" WHERE id=$1`, userID); got != 1 {
		t.Fatalf("unrelated user was removed: %d", got)
	}
}
