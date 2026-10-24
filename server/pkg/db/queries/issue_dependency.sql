-- Graph writers serialize per workspace. This also fences claims: an edge
-- cannot be added after checking for active runs while a claim slips past it.
-- name: LockIssueDependencyGraph :exec
SELECT pg_advisory_xact_lock(hashtextextended(concat('issue_dependency:', sqlc.arg('workspace_id')::uuid), 0));

-- name: LockIssueDependencyGraphForClaim :exec
SELECT pg_advisory_xact_lock_shared(hashtextextended(concat('issue_dependency:', sqlc.arg('workspace_id')::uuid), 0));

-- name: ListIssueDependencies :many
SELECT d.* FROM issue_dependency d
JOIN issue a ON a.id = d.issue_id
JOIN issue b ON b.id = d.depends_on_issue_id AND b.workspace_id = a.workspace_id
WHERE a.workspace_id = @workspace_id
  AND (d.issue_id = @issue_id OR d.depends_on_issue_id = @issue_id)
  AND (@include_removed::boolean OR d.removed_at IS NULL)
ORDER BY d.created_at, d.id;

-- name: ListWorkspaceExecutionEdges :many
SELECT d.* FROM issue_dependency d
JOIN issue a ON a.id = d.issue_id
JOIN issue b ON b.id = d.depends_on_issue_id AND b.workspace_id = a.workspace_id
WHERE a.workspace_id = @workspace_id
  AND d.type IN ('blocks', 'blocked_by') AND d.removed_at IS NULL;

-- name: CreateIssueDependency :one
INSERT INTO issue_dependency (id, issue_id, depends_on_issue_id, type, condition, target_step, provenance)
VALUES (@id, @issue_id, @depends_on_issue_id, @type, @condition, @target_step, @provenance)
RETURNING *;

-- name: RemoveIssueDependency :one
UPDATE issue_dependency d SET removed_at = now(), provenance = d.provenance || @removal::jsonb
WHERE d.id = @id AND d.removed_at IS NULL
  AND (d.issue_id = @issue_id OR d.depends_on_issue_id = @issue_id)
  AND EXISTS (SELECT 1 FROM issue i WHERE i.id = d.issue_id AND i.workspace_id = @workspace_id)
RETURNING d.*;

-- name: GetIssueDependency :one
SELECT d.* FROM issue_dependency d
WHERE d.id = @id AND (d.issue_id = @issue_id OR d.depends_on_issue_id = @issue_id)
  AND EXISTS (SELECT 1 FROM issue i WHERE i.id = d.issue_id AND i.workspace_id = @workspace_id);

-- name: IssueDependencyWaits :many
SELECT dependency_id::uuid, upstream_id::uuid, reason::text FROM issue_dependency_waits(@issue_id::uuid, @step::text);

-- name: IssueDependenciesReady :one
SELECT issue_dependencies_ready(@issue_id::uuid, @step::text)::boolean;

-- name: IssueDependencyTransitionAllowed :one
SELECT issue_dependency_transition_allowed(@issue_id::uuid, @previous::text, @next::text)::boolean;

-- name: IssueEvidenceVersion :one
SELECT issue_evidence_version(@issue_id::uuid)::text;

-- name: HasActiveDependencyRun :one
SELECT EXISTS(SELECT 1 FROM agent_task_queue
    WHERE issue_id = @issue_id AND status IN ('dispatched', 'running', 'waiting_local_directory'))::boolean;

-- name: AcceptIssueDependency :one
UPDATE issue_dependency SET provenance = provenance || @acceptance::jsonb || jsonb_build_object(
    'acceptance_history', COALESCE(provenance->'acceptance_history', '[]'::jsonb) || jsonb_build_array(@acceptance::jsonb))
WHERE id = @id AND removed_at IS NULL
RETURNING *;
