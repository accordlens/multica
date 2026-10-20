-- Retain a security tombstone when an attachment/session is deleted: a failed
-- object GC must not make its old /uploads URL become public again.
CREATE TABLE IF NOT EXISTS chat_protected_object (url text NOT NULL,attachment_id uuid NOT NULL);
INSERT INTO chat_protected_object(url,attachment_id)
SELECT a.url,a.id FROM attachment a WHERE (a.chat_session_id IS NOT NULL OR a.chat_message_id IS NOT NULL)
AND NOT EXISTS(SELECT 1 FROM chat_protected_object o WHERE o.url=a.url);
CREATE OR REPLACE FUNCTION protect_chat_object() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.chat_session_id IS NOT NULL OR NEW.chat_message_id IS NOT NULL THEN
   INSERT INTO chat_protected_object(url,attachment_id) SELECT NEW.url,NEW.id
   WHERE NOT EXISTS(SELECT 1 FROM chat_protected_object WHERE url=NEW.url) ON CONFLICT DO NOTHING;
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS protect_chat_object ON attachment;
CREATE TRIGGER protect_chat_object AFTER INSERT OR UPDATE ON attachment
FOR EACH ROW EXECUTE FUNCTION protect_chat_object();
