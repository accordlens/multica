CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_message_sequence ON chat_message(chat_session_id,message_seq);
