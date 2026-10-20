CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_message_thread ON chat_message(chat_session_id,root_message_id,message_seq,id);
