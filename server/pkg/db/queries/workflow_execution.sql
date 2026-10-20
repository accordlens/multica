-- Hybrid steps reuse the task queue, its claim/start fences and terminal outbox.
-- The source completion's CAS and this INSERT share one transaction. No event
-- handler performs a second enqueue after commit.
-- name: CreateWorkflowSuccessor :one
INSERT INTO agent_task_queue (
    id, agent_id, runtime_id, issue_id, status, priority, context,
    workflow_step, max_attempts, parent_task_id, force_fresh_session,
    handoff_note, originator_user_id, accountable_user_id, originator_source,
    delegated_from_task_id, trigger_evidence_kind, trigger_evidence_ref_id
)
SELECT @id::uuid, @agent_id::uuid, @runtime_id::uuid, source.issue_id,
    'queued', source.priority, @context::jsonb, @workflow_step::text,
    1, source.id, true, @handoff_note::text,
    source.originator_user_id, source.accountable_user_id, 'delegation',
    source.id, 'workflow_step', source.id
FROM agent_task_queue source
WHERE source.id = @source_id::uuid AND source.status = 'completed'
    AND lock_task_owner_rows(@agent_id::uuid, source.issue_id, @runtime_id::uuid)
RETURNING *;

-- name: LockWorkflowIssue :one
SELECT * FROM issue WHERE id = @id::uuid AND workspace_id = @workspace_id::uuid FOR UPDATE;

-- Failure/cancellation may already own the task row. Never wait on an issue
-- held by an issue-before-task completion: the caller rolls back and retries.
-- name: LockWorkflowIssueForFailure :one
SELECT * FROM issue WHERE id = @id::uuid AND workspace_id = @workspace_id::uuid FOR UPDATE NOWAIT;

-- The existing queue-expiry sweep also enforces a workflow's total budget,
-- including time spent waiting for an offline worker. Materialization bounds
-- the affected set under alternate plans and sparse table statistics.
-- name: ExpireWorkflowTasks :many
WITH victims AS MATERIALIZED (
    SELECT t.id FROM agent_task_queue t
    WHERE t.status IN ('queued','dispatched','waiting_local_directory','running')
      AND t.context->'hybrid_workflow'->>'deadline' IS NOT NULL
      AND (t.context->'hybrid_workflow'->>'deadline')::timestamptz < now()
    ORDER BY t.created_at, t.id LIMIT 100
    FOR UPDATE SKIP LOCKED
)
UPDATE agent_task_queue t SET status='failed',completed_at=now(),
    failure_reason='workflow_budget_exhausted',error='Workflow total time budget exhausted; no automatic replay',prepare_lease_expires_at=NULL
FROM victims v WHERE t.id=v.id AND t.status IN ('queued','dispatched','waiting_local_directory','running')
RETURNING t.*;

-- name: SetWorkflowTaskContext :one
UPDATE agent_task_queue SET context = COALESCE(context, '{}'::jsonb) || jsonb_build_object('hybrid_workflow', @execution::jsonb)
WHERE id = @id::uuid RETURNING *;

-- Status and assignment move atomically with the durable successor. Backend
-- writers do not dispatch another run through the ordinary assignment handler.
-- name: MoveWorkflowIssue :one
WITH wakeup_source AS MATERIALIZED (SELECT set_config('multica.source_task_id', COALESCE(sqlc.narg('source_task_id')::uuid::text, ''), true))
UPDATE issue i SET status = @status::text,
    assignee_type = 'agent', assignee_id = @agent_id::uuid,
    duplicate_of_issue_id = NULL,
    position = CASE WHEN status <> @status::text THEN (
        SELECT COALESCE(MIN(target.position), 0) - 1 FROM issue target
        WHERE target.workspace_id = i.workspace_id AND target.status = @status::text
    ) ELSE position END,
    revision = revision + 1, last_activity_at = now(), updated_at = now()
FROM wakeup_source
WHERE i.id = @id::uuid AND i.workspace_id = @workspace_id::uuid
RETURNING i.*;

-- name: LatestWorkflowApproval :one
SELECT t.* FROM agent_task_queue t JOIN agent a ON a.id = t.agent_id
WHERE t.issue_id = @issue_id::uuid AND a.workspace_id = @workspace_id::uuid
    AND t.context->'hybrid_workflow'->>'kind' = 'review'
ORDER BY t.created_at DESC, t.id DESC LIMIT 1;
