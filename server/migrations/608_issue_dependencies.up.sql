-- Extend the existing relation model. Legacy directions are read as stored;
-- new execution edges use blocked_by (dependent -> prerequisite). Existing
-- indexes cover both endpoints; no new index or foreign key is needed.
ALTER TABLE issue_dependency
    ADD COLUMN IF NOT EXISTS condition JSONB NOT NULL DEFAULT '{"kind":"done"}',
    ADD COLUMN IF NOT EXISTS target_step TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS provenance JSONB NOT NULL DEFAULT '{"source":"legacy"}',
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS removed_at TIMESTAMPTZ;

-- Acceptance binds the source issue state AND its recorded code/results.
-- Native run completion does not necessarily edit issue.revision. Including
-- run evidence and corrected provenance prevents a new SHA/report from reusing
-- an acceptance recorded before that evidence existed.
CREATE OR REPLACE FUNCTION issue_evidence_version(source UUID)
RETURNS TEXT LANGUAGE sql STABLE AS $$
    SELECT encode(sha256(convert_to(jsonb_build_object(
        'runs', COALESCE((
            SELECT jsonb_agg(jsonb_build_object(
                'run_id', t.id, 'evidence', t.result->'workflow_evidence', 'links', t.context->'provenance_links'
            ) ORDER BY t.id)
            FROM agent_task_queue t WHERE t.issue_id = source
              AND (t.result ? 'workflow_evidence' OR t.context ? 'provenance_links')
        ), '[]'::jsonb),
        'pull_requests', COALESCE((
            SELECT jsonb_agg(jsonb_build_object('provider', pr.provider, 'id', pr.id, 'head_sha', pr.head_sha) ORDER BY pr.provider, pr.id)
            FROM (
                SELECT 'github'::text AS provider, p.id, p.head_sha
                FROM issue_pull_request link JOIN github_pull_request p ON p.id = link.pull_request_id
                JOIN issue i ON i.id = link.issue_id AND i.workspace_id = p.workspace_id
                WHERE i.id = source
                UNION ALL
                SELECT p.provider, p.id, p.head_sha
                FROM issue_vcs_pull_request link JOIN vcs_pull_request p ON p.id = link.pull_request_id
                JOIN issue i ON i.id = link.issue_id AND i.workspace_id = p.workspace_id
                WHERE i.id = source
            ) pr
        ), '[]'::jsonb)
    )::text, 'UTF8')), 'hex');
$$;

-- The same read model drives the API and queue admission. Unknown conditions,
-- deleted prerequisites and malformed historical data fail closed. A run's
-- successful execution is a delivery fact, never implicit human acceptance.
CREATE OR REPLACE FUNCTION issue_dependency_waits(target UUID, step TEXT)
RETURNS TABLE(dependency_id UUID, upstream_id UUID, reason TEXT)
LANGUAGE sql STABLE AS $$
    SELECT d.id, source.id,
        CASE
            WHEN source.id IS NULL THEN 'unavailable'
            WHEN d.condition->>'kind' = 'done' THEN
                CASE WHEN COALESCE(s.category, source.status) = 'done' THEN '' ELSE 'awaiting_acceptance' END
            WHEN d.condition->>'kind' = 'in_review' THEN
                CASE WHEN source.status = COALESCE(NULLIF(d.condition->>'source_step', ''), 'in_review') OR COALESCE(s.category, source.status) = 'done'
                    THEN '' ELSE 'awaiting_delivery' END
            WHEN d.condition->>'kind' = 'accepted' THEN
                CASE WHEN (source.status = COALESCE(NULLIF(d.condition->>'source_step', ''), 'in_review') OR COALESCE(s.category, source.status) = 'done')
                    AND d.provenance->>'accepted_revision' = source.revision::text
                    AND d.provenance->>'accepted_evidence_version' = issue_evidence_version(source.id)
                    AND COALESCE(d.provenance->>'accepted_by', '') <> ''
                    THEN '' ELSE 'awaiting_human_acceptance' END
            WHEN d.condition->>'kind' = 'run_completed' THEN
                CASE WHEN EXISTS (
                    SELECT 1 FROM agent_task_queue t
                    WHERE t.id = CASE WHEN d.condition->>'run_id' ~* '^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$'
                        THEN (d.condition->>'run_id')::uuid END
                      AND t.issue_id = source.id AND t.status = 'completed'
                      AND (COALESCE(d.condition->>'source_step', '') = ''
                           OR t.context->>'workflow_entry_step' = d.condition->>'source_step')
                ) THEN '' ELSE 'awaiting_run' END
            ELSE 'unsupported_condition'
        END::text
    FROM issue_dependency d
    JOIN issue target_issue ON target_issue.id = target
    LEFT JOIN issue source ON source.id = CASE WHEN d.type = 'blocks' THEN d.issue_id ELSE d.depends_on_issue_id END
        AND source.workspace_id = target_issue.workspace_id
    LEFT JOIN issue_status s ON s.workspace_id = source.workspace_id AND s.key = source.status
    WHERE d.removed_at IS NULL AND d.type IN ('blocks', 'blocked_by')
      AND CASE WHEN d.type = 'blocks' THEN d.depends_on_issue_id ELSE d.issue_id END = target
      AND (d.target_step = '' OR d.target_step = step);
$$;

-- Preserve the definition and entry step that produced a native run. Updating
-- a workflow or moving an issue later cannot rewrite this historical source.
CREATE OR REPLACE FUNCTION issue_workflow_snapshot(target UUID)
RETURNS JSONB LANGUAGE sql STABLE AS $$
    SELECT jsonb_strip_nulls(jsonb_build_object(
        'workflow_entry_step', i.status, 'workflow_id', w.id, 'workflow_steps', w.steps
    ))
    FROM issue i LEFT JOIN project p ON p.id = i.project_id AND p.workspace_id = i.workspace_id
    LEFT JOIN LATERAL (
        SELECT workflow.id, workflow.steps FROM issue_workflow workflow
        WHERE workflow.workspace_id = i.workspace_id
          AND (workflow.id = p.workflow_id OR workflow.is_default)
        ORDER BY (workflow.id = p.workflow_id) DESC NULLS LAST, workflow.id
        LIMIT 1
    ) w ON p.id IS NOT NULL
    WHERE i.id = target;
$$;

CREATE OR REPLACE FUNCTION snapshot_issue_run_workflow()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.issue_id IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.issue_id IS NULL) THEN
        NEW.context := COALESCE(NEW.context, '{}'::jsonb) || COALESCE(issue_workflow_snapshot(NEW.issue_id), '{}'::jsonb);
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS snapshot_issue_run_workflow ON agent_task_queue;
CREATE TRIGGER snapshot_issue_run_workflow
BEFORE INSERT OR UPDATE OF issue_id ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION snapshot_issue_run_workflow();

CREATE OR REPLACE FUNCTION issue_dependency_transition_allowed(target UUID, previous TEXT, next TEXT)
RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT previous = next OR (next <> 'in_review' AND COALESCE((
        SELECT s.category FROM issue i JOIN issue_status s ON s.workspace_id = i.workspace_id AND s.key = next WHERE i.id = target
    ), next) <> 'done') OR (
        NOT EXISTS (SELECT 1 FROM issue_dependency_waits(target, previous) WHERE reason <> '')
        AND NOT EXISTS (SELECT 1 FROM issue_dependency_waits(target, next) WHERE reason <> '')
    );
$$;

CREATE OR REPLACE FUNCTION issue_dependencies_ready(target UUID, step TEXT)
RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT NOT EXISTS (SELECT 1 FROM issue_dependency_waits(target, step) WHERE reason <> '')
      AND NOT EXISTS (
          SELECT 1 FROM issue i CROSS JOIN LATERAL issue_dependency_waits(target, i.status) w
          WHERE i.id = target AND w.reason <> ''
      )
      AND NOT EXISTS (
          SELECT 1 FROM issue i
          LEFT JOIN issue_status s ON s.workspace_id = i.workspace_id AND s.key = i.status
          WHERE i.id = target AND COALESCE(s.category, i.status) IN ('done', 'closed', 'cancelled')
            AND EXISTS (SELECT 1 FROM issue_dependency_waits(target, step))
      );
$$;
