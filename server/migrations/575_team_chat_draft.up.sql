CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_draft ON chat_draft(chat_session_id,user_id,root_message_id) NULLS NOT DISTINCT;
