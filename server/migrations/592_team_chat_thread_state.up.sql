CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_thread_state ON chat_thread_state(chat_session_id,root_message_id,user_id);
