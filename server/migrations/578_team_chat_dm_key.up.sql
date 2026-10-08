CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_dm_key ON chat_session(workspace_id,dm_key) WHERE kind IN ('dm','self_dm') AND dm_key IS NOT NULL;
