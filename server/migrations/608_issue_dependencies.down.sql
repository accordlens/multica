DROP TRIGGER IF EXISTS snapshot_issue_run_workflow ON agent_task_queue;
DROP FUNCTION IF EXISTS snapshot_issue_run_workflow();
DROP FUNCTION IF EXISTS issue_workflow_snapshot(UUID);
DROP FUNCTION IF EXISTS issue_dependency_transition_allowed(UUID, TEXT, TEXT);
DROP FUNCTION IF EXISTS issue_dependencies_ready(UUID, TEXT);
DROP FUNCTION IF EXISTS issue_dependency_waits(UUID, TEXT);
DROP FUNCTION IF EXISTS issue_evidence_version(UUID);
ALTER TABLE issue_dependency
    DROP COLUMN IF EXISTS condition,
    DROP COLUMN IF EXISTS target_step,
    DROP COLUMN IF EXISTS provenance,
    DROP COLUMN IF EXISTS created_at,
    DROP COLUMN IF EXISTS removed_at;
