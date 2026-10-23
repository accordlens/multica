-- Native task rows remain the owner of run/step/revision provenance.
-- name: ListIssueProvenanceRuns :many
SELECT t.id, t.workflow_step, t.status, t.context, t.result, t.created_at
FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
WHERE i.id = @issue_id AND i.workspace_id = @workspace_id
  AND t.chat_session_id IS NULL
  AND EXISTS (SELECT 1 FROM chat_protected_task p WHERE p.task_id = t.id AND NOT p.is_private)
ORDER BY t.created_at DESC, t.id DESC LIMIT 101 OFFSET @page_offset::int;

-- name: GetIssueProvenanceRun :one
SELECT t.id, t.context FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
WHERE t.id = @run_id AND i.id = @issue_id AND i.workspace_id = @workspace_id
  AND t.chat_session_id IS NULL
  AND EXISTS (SELECT 1 FROM chat_protected_task p WHERE p.task_id = t.id AND NOT p.is_private);

-- name: LockIssueProvenanceRun :one
SELECT t.* FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
WHERE t.id = @run_id AND i.id = @issue_id AND i.workspace_id = @workspace_id
  AND t.chat_session_id IS NULL
  AND EXISTS (SELECT 1 FROM chat_protected_task p WHERE p.task_id = t.id AND NOT p.is_private)
FOR UPDATE OF t;

-- name: SetIssueRunProvenanceLinks :exec
UPDATE agent_task_queue SET context = jsonb_set(COALESCE(context, '{}'::jsonb), '{provenance_links}', @links::jsonb)
WHERE id = @run_id;

-- name: ListIssueProvenanceArtifacts :many
SELECT a.id, a.filename, a.task_id, a.created_at FROM attachment a
WHERE a.workspace_id = @workspace_id AND a.chat_session_id IS NULL AND a.chat_message_id IS NULL AND a.source_context_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM chat_protected_object p WHERE p.attachment_id = a.id)
  AND (a.task_id IS NULL OR EXISTS (
    SELECT 1 FROM agent_task_queue t JOIN chat_protected_task p ON p.task_id = t.id AND NOT p.is_private
    WHERE t.id = a.task_id AND t.issue_id = @issue_id AND t.chat_session_id IS NULL
  ))
  AND (a.issue_id = @issue_id OR EXISTS (
    SELECT 1 FROM comment c WHERE c.id = a.comment_id AND c.issue_id = @issue_id AND c.workspace_id = @workspace_id
  ))
ORDER BY a.created_at DESC, a.id DESC LIMIT 101 OFFSET @page_offset::int;

-- name: GetIssueProvenanceArtifact :one
SELECT a.* FROM attachment a
WHERE a.id = @id AND a.workspace_id = @workspace_id
  AND a.chat_session_id IS NULL AND a.chat_message_id IS NULL AND a.source_context_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM chat_protected_object p WHERE p.attachment_id = a.id)
  AND (a.issue_id = @issue_id OR EXISTS (
    SELECT 1 FROM comment c WHERE c.id = a.comment_id AND c.issue_id = @issue_id AND c.workspace_id = @workspace_id
  ))
  AND (a.task_id IS NULL OR (a.task_id = @run_id AND EXISTS (
    SELECT 1 FROM agent_task_queue t JOIN chat_protected_task p ON p.task_id = t.id AND NOT p.is_private
    WHERE t.id = a.task_id AND t.issue_id = @issue_id AND t.chat_session_id IS NULL
  )));
