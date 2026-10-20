CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_read_state ON chat_read_state(chat_session_id,user_id,root_message_id) NULLS NOT DISTINCT;
