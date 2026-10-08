CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_participant_identity ON chat_participant(chat_session_id,actor_type,actor_id);
