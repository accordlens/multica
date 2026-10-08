-- Preserve the privacy boundary when the historical FK clears chat_session_id.
-- Pre-existing unscoped tasks cannot be distinguished from deleted chat tasks:
-- quarantine their human-facing content until an operator classifies the origin.
CREATE TABLE IF NOT EXISTS chat_protected_task (task_id uuid NOT NULL,chat_session_id uuid,is_private boolean NOT NULL DEFAULT true);
INSERT INTO chat_protected_task(task_id,chat_session_id,is_private)
SELECT t.id,t.chat_session_id,(t.chat_session_id IS NOT NULL OR t.issue_id IS NULL) FROM agent_task_queue t
WHERE NOT EXISTS(SELECT 1 FROM chat_protected_task p WHERE p.task_id=t.id);
CREATE OR REPLACE FUNCTION protect_chat_task() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE original_scope uuid; private_scope boolean;
BEGIN
 IF NEW.chat_session_id IS NOT NULL THEN
   SELECT chat_session_id,is_private INTO original_scope,private_scope FROM chat_protected_task WHERE task_id=NEW.id;
   IF FOUND AND private_scope AND original_scope IS DISTINCT FROM NEW.chat_session_id THEN
     RAISE EXCEPTION 'task conversation scope is immutable';
   END IF;
   UPDATE chat_protected_task SET chat_session_id=NEW.chat_session_id,is_private=true WHERE task_id=NEW.id AND NOT is_private;
   INSERT INTO chat_protected_task(task_id,chat_session_id,is_private) SELECT NEW.id,NEW.chat_session_id,true
   WHERE NOT EXISTS(SELECT 1 FROM chat_protected_task WHERE task_id=NEW.id) ON CONFLICT DO NOTHING;
 ELSIF TG_OP='INSERT' THEN
   INSERT INTO chat_protected_task(task_id,chat_session_id,is_private) SELECT NEW.id,NULL,false
   WHERE NOT EXISTS(SELECT 1 FROM chat_protected_task WHERE task_id=NEW.id) ON CONFLICT DO NOTHING;
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS protect_chat_task ON agent_task_queue;
CREATE TRIGGER protect_chat_task AFTER INSERT OR UPDATE ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION protect_chat_task();
