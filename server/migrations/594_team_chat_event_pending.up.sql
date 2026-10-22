CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_event_pending ON chat_event(created_at,id) WHERE published_at IS NULL;
