-- Expand the existing chat records. No new FK or cascading relationships.
ALTER TABLE "user" ADD COLUMN IF NOT EXISTS deactivated_at timestamptz;
ALTER TABLE chat_session
    ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'agent_dm',
    ADD COLUMN IF NOT EXISTS name text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS topic text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS description text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS is_general boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS acl_version bigint NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS last_message_seq bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_event_seq bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dm_key text;
ALTER TABLE chat_session ALTER COLUMN agent_id DROP NOT NULL;
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chat_session_kind_check' AND conrelid = 'chat_session'::regclass) THEN
        ALTER TABLE chat_session ADD CONSTRAINT chat_session_kind_check CHECK (
            kind IN ('agent_dm','public_channel','private_channel','dm','self_dm','group_dm')
            AND (kind != 'agent_dm' OR agent_id IS NOT NULL)
            AND (NOT is_general OR kind = 'public_channel'));
    END IF;
END $$;
ALTER TABLE chat_message
    ADD COLUMN IF NOT EXISTS actor_type text,
    ADD COLUMN IF NOT EXISTS actor_id uuid,
    ADD COLUMN IF NOT EXISTS message_seq bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS edited_at timestamptz,
    ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
    ADD COLUMN IF NOT EXISTS root_message_id uuid,
    ADD COLUMN IF NOT EXISTS broadcast_to_main boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS client_message_id uuid,
    ADD COLUMN IF NOT EXISTS content_format text NOT NULL DEFAULT 'markdown',
    ADD COLUMN IF NOT EXISTS system_kind text,
    ADD COLUMN IF NOT EXISTS source_message_id uuid;
CREATE TABLE IF NOT EXISTS chat_participant (
    chat_session_id uuid NOT NULL, actor_type text NOT NULL CHECK (actor_type IN ('member','agent')),
    actor_id uuid NOT NULL, role text NOT NULL DEFAULT 'participant' CHECK (role IN ('participant','manager')),
    joined_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz,
    membership_version bigint NOT NULL DEFAULT 1);
CREATE TABLE IF NOT EXISTS chat_read_state (
    chat_session_id uuid NOT NULL, user_id uuid NOT NULL, root_message_id uuid,
    up_to_message_seq bigint NOT NULL DEFAULT 0, revision bigint NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS chat_thread_state (
    chat_session_id uuid NOT NULL, root_message_id uuid NOT NULL, user_id uuid NOT NULL,
    followed boolean NOT NULL DEFAULT true, up_to_message_seq bigint NOT NULL DEFAULT 0,
    revision bigint NOT NULL DEFAULT 1);
CREATE TABLE IF NOT EXISTS chat_event (
    id uuid NOT NULL DEFAULT gen_random_uuid(), chat_session_id uuid NOT NULL,
    event_seq bigint NOT NULL, acl_version bigint NOT NULL, event_type text NOT NULL,
    actor_type text NOT NULL, actor_id uuid NOT NULL, payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz);
CREATE TABLE IF NOT EXISTS chat_draft (
    chat_session_id uuid NOT NULL, user_id uuid NOT NULL, root_message_id uuid,
    revision bigint NOT NULL DEFAULT 1, content text NOT NULL DEFAULT '',
    attachment_ids uuid[] NOT NULL DEFAULT '{}', updated_at timestamptz NOT NULL DEFAULT now());
-- Retryable backfill: never overwrite authors/sequences/cursors once assigned.
WITH ordered AS (
    SELECT m.id, row_number() OVER (PARTITION BY m.chat_session_id ORDER BY m.created_at,m.id) AS seq,
           CASE WHEN m.role = 'assistant' THEN 'agent' ELSE 'member' END AS author_type,
           CASE WHEN m.role = 'assistant' THEN COALESCE(t.agent_id,s.agent_id) ELSE s.creator_id END AS author_id
    FROM chat_message m JOIN chat_session s ON s.id=m.chat_session_id
    LEFT JOIN agent_task_queue t ON t.id=m.task_id
)
UPDATE chat_message m SET message_seq=o.seq, actor_type=o.author_type, actor_id=o.author_id
FROM ordered o WHERE o.id=m.id AND m.message_seq=0;
UPDATE chat_session s SET last_message_seq=GREATEST(s.last_message_seq,
    COALESCE((SELECT max(message_seq) FROM chat_message m WHERE m.chat_session_id=s.id),0));
INSERT INTO chat_participant (chat_session_id,actor_type,actor_id,role,joined_at)
SELECT s.id,'member',s.creator_id,'manager',s.created_at FROM chat_session s
WHERE s.kind='agent_dm' AND NOT EXISTS (
    SELECT 1 FROM chat_participant p WHERE p.chat_session_id=s.id AND p.actor_type='member' AND p.actor_id=s.creator_id);
INSERT INTO chat_read_state (chat_session_id,user_id,up_to_message_seq)
SELECT s.id,s.creator_id,COALESCE((SELECT max(m.message_seq) FROM chat_message m
    WHERE m.chat_session_id=s.id AND m.created_at <= s.last_read_at),0)
FROM chat_session s WHERE s.kind='agent_dm' AND NOT EXISTS (
    SELECT 1 FROM chat_read_state rs WHERE rs.chat_session_id=s.id AND rs.user_id=s.creator_id AND rs.root_message_id IS NULL);
-- Legacy writers use the same records and sequence allocator as v2. This
-- trigger does not rewrite immutable task inputs or any existing message ID.
CREATE OR REPLACE FUNCTION assign_chat_message_identity() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s chat_session; historical_agent uuid;
BEGIN
    SELECT * INTO STRICT s FROM chat_session WHERE id=NEW.chat_session_id FOR NO KEY UPDATE;
    IF NEW.message_seq=0 THEN
        UPDATE chat_session SET last_message_seq=last_message_seq+1 WHERE id=s.id RETURNING last_message_seq INTO NEW.message_seq;
    END IF;
    IF NEW.actor_type IS NULL AND s.kind='agent_dm' THEN
        SELECT agent_id INTO historical_agent FROM agent_task_queue WHERE id=NEW.task_id;
        NEW.actor_type := CASE WHEN NEW.role='assistant' THEN 'agent' ELSE 'member' END;
        NEW.actor_id := CASE WHEN NEW.role='assistant' THEN COALESCE(historical_agent,s.agent_id) ELSE s.creator_id END;
    END IF;
    IF NEW.actor_type IS NULL OR NEW.actor_id IS NULL THEN RAISE EXCEPTION 'chat author required'; END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS chat_message_identity ON chat_message;
CREATE TRIGGER chat_message_identity BEFORE INSERT ON chat_message FOR EACH ROW EXECUTE FUNCTION assign_chat_message_identity();
CREATE OR REPLACE FUNCTION initialize_agent_chat_participant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.kind='agent_dm' THEN
        INSERT INTO chat_participant (chat_session_id,actor_type,actor_id,role)
        VALUES (NEW.id,'member',NEW.creator_id,'manager');
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS agent_chat_participant ON chat_session;
CREATE TRIGGER agent_chat_participant AFTER INSERT ON chat_session FOR EACH ROW EXECUTE FUNCTION initialize_agent_chat_participant();
