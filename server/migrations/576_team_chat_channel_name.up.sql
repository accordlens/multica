CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_channel_name ON chat_session(workspace_id,lower(name)) WHERE kind IN ('public_channel','private_channel');
