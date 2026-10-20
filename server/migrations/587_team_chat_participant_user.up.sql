CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_participant_user ON chat_participant(actor_id,chat_session_id) WHERE revoked_at IS NULL;
