CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_message_idempotency ON chat_message(chat_session_id,actor_type,actor_id,client_message_id) WHERE client_message_id IS NOT NULL;
