CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_general ON chat_session(workspace_id) WHERE is_general;
