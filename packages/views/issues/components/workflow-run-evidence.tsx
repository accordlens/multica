"use client";

import type { AgentTask } from "@multica/core/types";
import { useT } from "../../i18n";

/** Server evidence shares the run's existing issue ACL and Query lifecycle. */
export function WorkflowRunEvidence({ task }: { task: AgentTask }) {
  const { t } = useT("issues");
  const execution = task.workflow_execution;
  const evidence = task.workflow_evidence ?? execution?.evidence;
  if (!execution && !evidence) return null;
  const source = evidence?.source ?? execution?.source;
  return (
    <section className="min-w-0 md:col-span-2" aria-label={t(($) => $.workflow_run.evidence)}>
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 break-words">
        <dt className="text-muted-foreground">{t(($) => $.workflow_run.step)}</dt>
        <dd>{execution?.kind} · {task.workflow_step ?? execution?.step_key} · {evidence?.outcome ?? task.status}</dd>
        <dt className="text-muted-foreground">{t(($) => $.workflow_run.source)}</dt>
        <dd className="font-mono text-micro">{source?.sha}{source?.dirty_hash ? ` · ${source.dirty_hash}` : ""}</dd>
        <dt className="text-muted-foreground">{t(($) => $.workflow_run.definition)}</dt>
        <dd className="font-mono text-micro">{evidence?.workflow_version ?? execution?.workflow_version}</dd>
        {execution && <>
          <dt className="text-muted-foreground">{t(($) => $.workflow_run.script)}</dt>
          <dd>{execution.script.id} · {execution.script.version}</dd>
          <dt className="text-muted-foreground">{t(($) => $.workflow_run.repairs)}</dt>
          <dd>{execution.repairs}/{execution.script.max_repairs}</dd>
        </>}
        {evidence?.report_id && <>
          <dt className="text-muted-foreground">{t(($) => $.workflow_run.report)}</dt>
          <dd className="font-mono text-micro">{evidence.report_id} · {evidence.report_hash}</dd>
        </>}
      </dl>
      {(execution?.block_reason || evidence?.reason) && <p className="mt-2 text-destructive">{execution?.block_reason || evidence?.reason}</p>}
      {execution?.review_verdict && <p className="mt-2 font-medium">{execution.review_verdict}</p>}
      {evidence?.report && <details className="mt-2">
        <summary className="cursor-pointer">{t(($) => $.workflow_run.report)}</summary>
        <pre className="mt-1 max-h-52 overflow-auto whitespace-pre-wrap break-all text-micro">{evidence.report}</pre>
      </details>}
      {evidence?.log && <details className="mt-2">
        <summary className="cursor-pointer">{t(($) => $.workflow_run.log)}</summary>
        <pre className="mt-1 max-h-52 overflow-auto whitespace-pre-wrap break-all text-micro">{evidence.log}</pre>
      </details>}
    </section>
  );
}
