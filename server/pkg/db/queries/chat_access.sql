-- Every transport reads current membership; no positive ACL cache.
-- name: GetChatProtectedTask :one
SELECT * FROM chat_protected_task WHERE task_id= @task_id AND is_private;

-- name: ListChatProtectedTasks :many
SELECT * FROM chat_protected_task WHERE task_id=ANY(@ids::uuid[]) AND is_private;

-- name: GetChatAccess :one
SELECT s.id, s.workspace_id, s.kind, s.agent_id, s.creator_id, s.status,
       s.revision, s.acl_version, s.is_general, m.role AS workspace_role,
       u.email, COALESCE(p.role,'')::text AS participant_role
FROM chat_session s
JOIN member m ON m.workspace_id=s.workspace_id AND m.user_id= @user_id
JOIN "user" u ON u.id=m.user_id AND u.deactivated_at IS NULL
LEFT JOIN chat_participant p ON p.chat_session_id=s.id AND p.actor_type='member'
    AND p.actor_id=m.user_id AND p.revoked_at IS NULL
WHERE s.id= @chat_session_id AND s.workspace_id= @workspace_id
    AND ((s.kind='agent_dm' AND s.creator_id=m.user_id AND p.actor_id IS NOT NULL)
        OR s.kind='public_channel'
        OR (s.kind IN ('private_channel','dm','self_dm','group_dm') AND p.actor_id IS NOT NULL));

-- name: ListChatRecipientIDs :many
-- A recipient candidate is reauthorized at socket delivery, including relay replay.
SELECT m.user_id FROM chat_session s
JOIN member m ON m.workspace_id=s.workspace_id
JOIN "user" u ON u.id=m.user_id AND u.deactivated_at IS NULL
LEFT JOIN chat_participant p ON p.chat_session_id=s.id AND p.actor_type='member'
    AND p.actor_id=m.user_id AND p.revoked_at IS NULL
WHERE s.id= @chat_session_id AND s.workspace_id= @workspace_id
    AND ((s.kind='agent_dm' AND s.creator_id=m.user_id AND p.actor_id IS NOT NULL)
        OR s.kind='public_channel'
        OR (s.kind IN ('private_channel','dm','self_dm','group_dm') AND p.actor_id IS NOT NULL));

-- name: LockChatConversation :one
SELECT * FROM chat_session WHERE id= @id AND workspace_id= @workspace_id FOR UPDATE;

-- name: GetChatConversation :one
SELECT * FROM chat_session WHERE id= @id AND workspace_id= @workspace_id;

-- name: ChangeChatParticipant :exec
INSERT INTO chat_participant(chat_session_id,actor_type,actor_id,role,revoked_at)
VALUES (@chat_session_id,'member',@user_id,@role,sqlc.narg(revoked_at))
ON CONFLICT(chat_session_id,actor_type,actor_id) DO UPDATE
SET role=EXCLUDED.role, revoked_at=EXCLUDED.revoked_at,
    joined_at=CASE WHEN EXCLUDED.revoked_at IS NULL THEN now() ELSE chat_participant.joined_at END,
    membership_version=chat_participant.membership_version+1;

-- name: BumpChatACL :one
UPDATE chat_session SET acl_version=acl_version+1,revision=revision+1
WHERE id= @id RETURNING *;

-- name: UpdateChatConversationMetadata :one
UPDATE chat_session SET name= @name,topic= @topic,description= @description,
    kind= @kind,revision=revision+1,
    acl_version=acl_version+CASE WHEN kind<> @kind THEN 1 ELSE 0 END
WHERE id= @id RETURNING *;

-- name: CreateHumanChatMessage :one
INSERT INTO chat_message(id,chat_session_id,role,content,actor_type,actor_id,root_message_id,client_message_id)
VALUES (@id,@chat_session_id,'user',@content,'member',@user_id,sqlc.narg(root_message_id),@client_message_id)
RETURNING *;

-- name: GetChatMessageByID :one
SELECT * FROM chat_message WHERE id= @id;

-- name: ListChatMessagesBySequence :many
SELECT * FROM chat_message
WHERE chat_session_id= @chat_session_id
  AND root_message_id IS NULL
  AND message_seq<= @snapshot_head AND message_seq< @before_seq
ORDER BY message_seq DESC LIMIT @page_limit;

-- name: ListChatThreadMessagesBySequence :many
SELECT * FROM chat_message
WHERE chat_session_id= @chat_session_id AND root_message_id= @root_message_id
  AND message_seq<= @snapshot_head AND message_seq< @before_seq
ORDER BY message_seq DESC LIMIT @page_limit;

-- name: SetChatReadState :one
INSERT INTO chat_read_state(chat_session_id,user_id,root_message_id,up_to_message_seq)
VALUES (@chat_session_id,@user_id,sqlc.narg(root_message_id),@up_to_message_seq)
ON CONFLICT(chat_session_id,user_id,root_message_id) DO UPDATE
SET up_to_message_seq=GREATEST(chat_read_state.up_to_message_seq,EXCLUDED.up_to_message_seq),
    revision=chat_read_state.revision+1,updated_at=now()
RETURNING *;

-- name: RevokeChatWorkspaceMember :exec
-- Caller locks the affected conversations first, in ID order.
WITH revoked AS (
 UPDATE chat_participant p SET revoked_at=now(),membership_version=membership_version+1
 FROM chat_session s WHERE s.id=p.chat_session_id AND s.workspace_id= @workspace_id
 AND p.actor_type='member' AND p.actor_id= @user_id AND p.revoked_at IS NULL
 RETURNING p.chat_session_id
)
UPDATE chat_session SET acl_version=acl_version+1,revision=revision+1
WHERE id IN (SELECT chat_session_id FROM revoked);

-- name: GetAttachmentByStorageSuffix :one
-- Local uploads, including legacy chat files, must go through conversation ACL.
SELECT * FROM attachment WHERE url= @relative_url OR right(url,length(@relative_url)::int)= @relative_url LIMIT 1;

-- name: GetChatProtectedObjectBySuffix :one
SELECT attachment_id FROM chat_protected_object
WHERE url = @relative_url OR right(url,length(@relative_url)::int) = @relative_url LIMIT 1;
