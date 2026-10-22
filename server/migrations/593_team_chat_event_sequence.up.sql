CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_event_sequence ON chat_event(chat_session_id,event_seq);
