package migrations

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTeamChatMigration_EmptyLegacyAndRetry(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("DATABASE_URL required for C2 migration evidence")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			schema := "chat_migration_" + fmt.Sprintf("%x", uuid.New())
			if _, err = conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
				t.Fatal(err)
			}
			defer pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
			if _, err = conn.Exec(ctx, "SET search_path TO "+schema); err != nil {
				t.Fatal(err)
			}
			if _, err = conn.Exec(ctx, `
CREATE TABLE "user"(id uuid PRIMARY KEY);
CREATE TABLE chat_session(id uuid PRIMARY KEY,workspace_id uuid NOT NULL,agent_id uuid NOT NULL,creator_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),last_read_at timestamptz NOT NULL DEFAULT now(),status text NOT NULL DEFAULT 'active');
CREATE TABLE chat_message(id uuid PRIMARY KEY,chat_session_id uuid NOT NULL,role text NOT NULL,content text NOT NULL,task_id uuid,created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE agent_task_queue(id uuid PRIMARY KEY,agent_id uuid NOT NULL,chat_session_id uuid,chat_input_task_id uuid,context jsonb,issue_id uuid);
CREATE TABLE attachment(id uuid PRIMARY KEY,chat_session_id uuid,chat_message_id uuid,task_id uuid,url text NOT NULL);
`); err != nil {
				t.Fatal(err)
			}
			if legacy {
				if _, err = conn.Exec(ctx, `
INSERT INTO "user" VALUES('00000000-0000-4000-8000-000000000001');
INSERT INTO chat_session(id,workspace_id,agent_id,creator_id,last_read_at) VALUES('00000000-0000-4000-8000-000000000005','00000000-0000-4000-8000-000000000009','00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000001','2026-01-01');
INSERT INTO agent_task_queue(id,agent_id,chat_session_id,chat_input_task_id,context) VALUES('00000000-0000-4000-8000-000000000004','00000000-0000-4000-8000-000000000002','00000000-0000-4000-8000-000000000005','00000000-0000-4000-8000-000000000004','{"input":"immutable"}');
INSERT INTO agent_task_queue(id,agent_id,issue_id) VALUES('00000000-0000-4000-8000-000000000010','00000000-0000-4000-8000-000000000002',NULL),('00000000-0000-4000-8000-000000000011','00000000-0000-4000-8000-000000000002','00000000-0000-4000-8000-000000000009');
INSERT INTO chat_message VALUES('00000000-0000-4000-8000-000000000006','00000000-0000-4000-8000-000000000005','user','input','00000000-0000-4000-8000-000000000004','2026-01-01'),('00000000-0000-4000-8000-000000000007','00000000-0000-4000-8000-000000000005','assistant','reply','00000000-0000-4000-8000-000000000004','2026-01-02');
INSERT INTO attachment VALUES('00000000-0000-4000-8000-000000000008','00000000-0000-4000-8000-000000000005','00000000-0000-4000-8000-000000000006','00000000-0000-4000-8000-000000000004','/uploads/legacy-private.txt');
`); err != nil {
					t.Fatal(err)
				}
			}
			// Whole existing records must survive, not just totals or new columns.
			var before string
			snapshot := `SELECT jsonb_build_object('sessions',COALESCE((SELECT jsonb_agg(jsonb_build_array(id,workspace_id,agent_id,creator_id,last_read_at)) FROM chat_session),'[]'), 'messages',COALESCE((SELECT jsonb_agg(jsonb_build_array(id,chat_session_id,role,content,task_id,created_at)) FROM chat_message),'[]'), 'tasks',COALESCE((SELECT jsonb_agg(to_jsonb(t)) FROM agent_task_queue t),'[]'), 'attachments',COALESCE((SELECT jsonb_agg(to_jsonb(a)) FROM attachment a),'[]'))::text`
			if err = conn.QueryRow(ctx, snapshot).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				applyMigrationFile(t, ctx, conn.Conn(), "565_team_chat_expand.up.sql")
			}
			for _, file := range []string{"566_team_chat_participant_identity.up.sql", "567_team_chat_participant_user.up.sql", "568_team_chat_message_sequence.up.sql", "569_team_chat_message_thread.up.sql", "570_team_chat_message_idempotency.up.sql", "571_team_chat_read_state.up.sql", "572_team_chat_thread_state.up.sql", "573_team_chat_event_sequence.up.sql", "574_team_chat_event_pending.up.sql", "575_team_chat_draft.up.sql", "576_team_chat_channel_name.up.sql", "577_team_chat_general.up.sql", "578_team_chat_dm_key.up.sql", "579_team_chat_protected_objects.up.sql", "580_team_chat_protected_objects_index.up.sql", "581_team_chat_protected_tasks.up.sql", "582_team_chat_protected_tasks_index.up.sql"} {
				for i := 0; i < 2; i++ {
					applyMigrationFile(t, ctx, conn.Conn(), file)
				}
			}
			var after string
			if err = conn.QueryRow(ctx, snapshot).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatalf("legacy records changed:\n%s\n%s", before, after)
			}
			if legacy {
				var privateBound, unknownDenied, issuePublic bool
				if err = conn.QueryRow(ctx, `SELECT (SELECT is_private AND chat_session_id IS NOT NULL FROM chat_protected_task WHERE task_id='00000000-0000-4000-8000-000000000004'),(SELECT is_private AND chat_session_id IS NULL FROM chat_protected_task WHERE task_id='00000000-0000-4000-8000-000000000010'),(SELECT NOT is_private FROM chat_protected_task WHERE task_id='00000000-0000-4000-8000-000000000011')`).Scan(&privateBound, &unknownDenied, &issuePublic); err != nil || !privateBound || !unknownDenied || !issuePublic {
					t.Fatalf("historical task classification: bound=%v quarantined=%v issuePublic=%v error=%v", privateBound, unknownDenied, issuePublic, err)
				}
				var authorType, author, agent string
				var unread, readSeq, participants int
				if err = conn.QueryRow(ctx, `SELECT actor_type,actor_id::text,(SELECT actor_id::text FROM chat_message WHERE role='assistant') FROM chat_message WHERE role='user'`).Scan(&authorType, &author, &agent); err != nil {
					t.Fatal(err)
				}
				if authorType != "member" || author != "00000000-0000-4000-8000-000000000001" || agent != "00000000-0000-4000-8000-000000000002" {
					t.Fatalf("historical authors: %s %s %s", authorType, author, agent)
				}
				if err = conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM chat_message m JOIN chat_session s ON s.id=m.chat_session_id WHERE m.role='assistant' AND m.created_at>s.last_read_at),(SELECT up_to_message_seq FROM chat_read_state),(SELECT count(*) FROM chat_participant)`).Scan(&unread, &readSeq, &participants); err != nil {
					t.Fatal(err)
				}
				if unread != 1 || readSeq != 1 || participants != 1 {
					t.Fatalf("retry/read backfill changed: unread=%d read=%d participants=%d", unread, readSeq, participants)
				}
			}
			newPublicTask := uuid.NewString()
			if _, err = conn.Exec(ctx, `INSERT INTO agent_task_queue(id,agent_id) VALUES($1,$1)`, newPublicTask); err != nil {
				t.Fatal(err)
			}
			applyMigrationFile(t, ctx, conn.Conn(), "581_team_chat_protected_tasks.up.sql")
			var newPublic bool
			if err = conn.QueryRow(ctx, `SELECT NOT is_private FROM chat_protected_task WHERE task_id=$1`, newPublicTask).Scan(&newPublic); err != nil || !newPublic {
				t.Fatalf("retry quarantined new ordinary task: public=%v error=%v", newPublic, err)
			}
			// Verify a real populated cursor plan, after the preservation checks.
			session := uuid.NewString()
			if _, err = conn.Exec(ctx, `INSERT INTO chat_session(id,workspace_id,agent_id,creator_id) VALUES($1,$1,$1,$1)`, session); err != nil {
				t.Fatal(err)
			}
			if _, err = conn.Exec(ctx, `INSERT INTO chat_message(id,chat_session_id,role,content,message_seq) SELECT gen_random_uuid(),$1,'user','history',n FROM generate_series(1,5000) n`, session); err != nil {
				t.Fatal(err)
			}
			if _, err = conn.Exec(ctx, `ANALYZE chat_message`); err != nil {
				t.Fatal(err)
			}
			rows, err := conn.Query(ctx, `EXPLAIN(ANALYZE,BUFFERS) SELECT * FROM chat_message WHERE chat_session_id=$1 AND root_message_id IS NULL AND message_seq<=5000 AND message_seq<4000 ORDER BY message_seq DESC LIMIT 51`, session)
			if err != nil {
				t.Fatal(err)
			}
			plan := ""
			for rows.Next() {
				var line string
				if err = rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan += line + "\n"
			}
			rows.Close()
			if rows.Err() != nil {
				t.Fatal(rows.Err())
			}
			if !(strings.Contains(plan, "idx_chat_message_thread") || strings.Contains(plan, "idx_chat_message_sequence")) || strings.Contains(plan, "Sort") {
				t.Fatalf("cursor plan must use a matching sequence index without sort:\n%s", plan)
			}
			t.Logf("5000-row cursor plan:\n%s", plan)

		})
	}
}
