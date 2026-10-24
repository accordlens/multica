"use client";

import { useId, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Plus, Unlink, X } from "lucide-react";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { issueRelationsOptions, issueProvenanceOptions, useRemoveIssueProvenance, useAcceptIssueRelation, useCreateIssueRelation, useRemoveIssueRelation } from "@multica/core/issues/relations";
import type { DependencyConditionKind, IssueRelationType } from "@multica/core/types/issue-relation";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Field, FieldGroup, FieldLabel, FieldSet } from "@multica/ui/components/ui/field";
import { Input } from "@multica/ui/components/ui/input";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@multica/ui/components/ui/select";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { AppLink } from "../../navigation";
import { useT } from "../../i18n";

export function IssueRelationsSection({ issueId }: { issueId: string }) {
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const { t } = useT("issues");
  const formId = useId();
  const query = useQuery(issueRelationsOptions(wsId, issueId));
  const provenance = useQuery(issueProvenanceOptions(wsId, issueId));
  const removeProvenance = useRemoveIssueProvenance(wsId, issueId);
  const create = useCreateIssueRelation(wsId, issueId);
  const remove = useRemoveIssueRelation(wsId, issueId);
  const accept = useAcceptIssueRelation(wsId, issueId);
  const [editing, setEditing] = useState(false);
  const [other, setOther] = useState("");
  const [type, setType] = useState<IssueRelationType>("blocked_by");
  const [condition, setCondition] = useState<DependencyConditionKind>("done");
  const [runId, setRunId] = useState("");
  const [step, setStep] = useState("");
  const [error, setError] = useState<string | null>(null);
  const busy = create.isPending || remove.isPending || accept.isPending;
  const typeLabels: Record<IssueRelationType, string> = {
    blocked_by: t(($) => $.relations.blocked_by),
    blocks: t(($) => $.relations.blocks),
    related: t(($) => $.relations.related),
  };
  const conditionLabels: Record<DependencyConditionKind, string> = {
    done: t(($) => $.relations.done),
    in_review: t(($) => $.relations.in_review),
    accepted: t(($) => $.relations.accepted),
    run_completed: t(($) => $.relations.run_completed),
  };
  const reasons: Record<string, string> = {
    awaiting_acceptance: t(($) => $.relations.awaiting_acceptance),
    awaiting_delivery: t(($) => $.relations.awaiting_delivery),
    awaiting_human_acceptance: t(($) => $.relations.awaiting_human_acceptance),
    awaiting_run: t(($) => $.relations.awaiting_run),
    unavailable: t(($) => $.relations.unavailable),
    issue_closed: t(($) => $.relations.issue_closed),
  };
  const types = Object.entries(typeLabels).map(([value, label]) => ({ value, label }));
  const conditions = Object.entries(conditionLabels).map(([value, label]) => ({ value, label }));

  async function add() {
    if (create.isPending) return;
    setError(null);
    try {
      await create.mutateAsync({ issue_id: other.trim(), type, condition: { kind: type === "related" ? "done" : condition, ...(condition === "run_completed" && type !== "related" ? { run_id: runId.trim() } : {}) }, target_step: type === "related" ? "" : step.trim() });
      setOther("");
      setRunId("");
      setEditing(false);
    } catch (err) { setError(err instanceof Error ? err.message : t(($) => $.relations.save_error)); }
  }

  return (
    <section className="flex flex-col gap-2 py-3" aria-label={t(($) => $.relations.title)}>
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-body font-medium">{t(($) => $.relations.title)}</h3>
        <Button size="icon-xs" variant="ghost" disabled={create.isPending} aria-label={editing ? t(($) => $.relations.cancel) : t(($) => $.relations.add)} onClick={() => { setEditing(!editing); setError(null); }}>
          {editing ? <X /> : <Plus />}
        </Button>
      </div>
      {query.isPending && <Skeleton className="h-7 w-full" />}
      {(query.isError || query.data?.available === false) && (
        <div className="flex items-center gap-2 text-body text-muted-foreground" role="status">
          {t(($) => $.relations.load_error)}
          <Button size="xs" variant="ghost" onClick={() => void query.refetch()}>{t(($) => $.relations.retry)}</Button>
        </div>
      )}
      {query.data?.available && (query.data.reason || query.data.relations.some((row) => row.type === "blocked_by")) && (
        <p className="text-caption text-muted-foreground" role="status">{query.data.ready ? t(($) => $.relations.ready) : reasons[query.data.reason] ?? t(($) => $.relations.waiting)}</p>
      )}
      {query.data?.available && query.data.relations.map((row) => (
        <div key={row.id} className="flex min-w-0 items-start gap-2 rounded-md border p-2">
          <div className="flex min-w-0 flex-1 flex-col gap-1">
            <span className="text-caption text-muted-foreground">{typeLabels[row.type as IssueRelationType] ?? t(($) => $.relations.unknown_type)}</span>
            <AppLink href={paths.issueDetail(row.issue.id)} newTabTitle={row.issue.identifier} className="truncate text-body hover:underline" title={row.issue.title}>
              <span className="text-muted-foreground">{row.issue.identifier}</span>{" "}{row.issue.title}
            </AppLink>
            {row.type !== "related" && <div className="flex flex-wrap items-center gap-1 text-caption">
              <Badge variant={row.satisfied ? "secondary" : "outline"}>{row.satisfied ? t(($) => $.relations.met) : reasons[row.reason] ?? t(($) => $.relations.unsupported)}</Badge>
              <span className="text-muted-foreground">{conditionLabels[row.condition.kind as DependencyConditionKind] ?? t(($) => $.relations.unsupported)}</span>
              {row.target_step && <span className="text-muted-foreground">{t(($) => $.relations.step_value, { step: row.target_step })}</span>}
            </div>}
            {row.type === "blocked_by" && row.condition.kind === "accepted" && !!row.source_evidence_version && !row.satisfied && (row.issue.status === (row.condition.source_step || "in_review") || row.issue.status === "done" || row.issue.status_category === "done") && (
              <Button size="xs" variant="outline" disabled={busy} onClick={async () => {
                setError(null);
                try { await accept.mutateAsync({ id: row.id, revision: row.source_revision, evidenceVersion:row.source_evidence_version }); }
                catch (err) { setError(err instanceof Error ? err.message : t(($) => $.relations.save_error)); }
              }}>{t(($) => $.relations.accept_revision, { revision: row.source_revision })}</Button>
            )}
          </div>
          <Button size="icon-xs" variant="ghost" disabled={busy} aria-label={t(($) => $.relations.remove_link, { issue: row.issue.identifier })} onClick={async () => {
            setError(null);
            try { await remove.mutateAsync(row.id); }
            catch (err) { setError(err instanceof Error ? err.message : t(($) => $.relations.save_error)); }
          }}><Unlink /></Button>
        </div>
      ))}
      {editing && <form aria-label={t(($) => $.relations.add)} className="flex flex-col gap-3 rounded-md border p-3" onSubmit={(e) => { e.preventDefault(); void add(); }}>
        <FieldSet disabled={create.isPending}><FieldGroup>
          <Field><FieldLabel htmlFor={`${formId}-issue`}>{t(($) => $.relations.issue)}</FieldLabel><Input id={`${formId}-issue`} value={other} onChange={(e) => setOther(e.target.value)} required autoFocus /></Field>
          <Field><FieldLabel htmlFor={`${formId}-type`}>{t(($) => $.relations.relation)}</FieldLabel>
            <Select<IssueRelationType> value={type} onValueChange={(v) => { if (v) setType(v); }} items={types}>
              <SelectTrigger id={`${formId}-type`}><SelectValue /></SelectTrigger><SelectContent><SelectGroup>{types.map((item) => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectGroup></SelectContent>
            </Select>
          </Field>
          {type !== "related" && <>
            <Field><FieldLabel htmlFor={`${formId}-condition`}>{t(($) => $.relations.condition)}</FieldLabel>
              <Select<DependencyConditionKind> value={condition} onValueChange={(v) => { if (v) setCondition(v); }} items={conditions}>
                <SelectTrigger id={`${formId}-condition`}><SelectValue /></SelectTrigger><SelectContent><SelectGroup>{conditions.map((item) => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectGroup></SelectContent>
              </Select>
            </Field>
            {condition === "run_completed" && <Field><FieldLabel htmlFor={`${formId}-run`}>{t(($) => $.relations.source_run)}</FieldLabel><Input id={`${formId}-run`} value={runId} onChange={(e) => setRunId(e.target.value)} required /><p className="text-caption text-muted-foreground">{t(($) => $.relations.run_notice)}</p></Field>}
            <Field><FieldLabel htmlFor={`${formId}-step`}>{t(($) => $.relations.step)}</FieldLabel><Input id={`${formId}-step`} value={step} onChange={(e) => setStep(e.target.value)} /></Field>
          </>}
        </FieldGroup></FieldSet>
        <div className="flex justify-end gap-2"><Button type="button" size="sm" variant="outline" disabled={create.isPending} onClick={() => setEditing(false)}>{t(($) => $.relations.cancel)}</Button><Button type="submit" size="sm" disabled={busy} aria-busy={create.isPending}>{t(($) => $.relations.add)}</Button></div>
      </form>}
      {(provenance.isError || provenance.data?.available === false) && <p className="text-caption text-muted-foreground" role="status">{t(($) => $.relations.sources_unavailable)}</p>}
      {provenance.data?.links.length ? <div className="flex flex-col gap-2">
        <h4 className="text-caption font-medium text-muted-foreground">{t(($) => $.relations.sources)}</h4>
        {provenance.data.links.map((link) => <div key={`${link.run_id}:${link.id}`} className="flex min-w-0 items-start gap-2 text-caption">
          <div className="flex min-w-0 flex-1 flex-col gap-0.5">
            {link.kind === "pull_request" && link.url?.startsWith("https://")
              ? <a href={link.url} target="_blank" rel="noopener noreferrer" className="truncate hover:underline">{link.url}</a>
              : <code className="truncate" title={link.commit_sha ?? link.sha256}>{link.commit_sha ?? provenance.data?.artifacts.find((file) => file.id === link.artifact_id)?.filename ?? link.artifact_id ?? link.kind}</code>}
            {link.dirty_hash && <span className="text-muted-foreground" title={link.dirty_hash}>{t(($) => $.relations.local_changes)}</span>}
            <span className="truncate text-muted-foreground" title={link.run_id}>{t(($) => $.relations.source_run_value, { run: link.run_id.slice(0, 8), step: link.step })}</span>
          </div>
          <Button size="icon-xs" variant="ghost" disabled={removeProvenance.isPending} aria-label={t(($) => $.relations.remove_source)} onClick={async () => {
            try { await removeProvenance.mutateAsync({ id: link.id, runId: link.run_id }); }
            catch (err) { setError(err instanceof Error ? err.message : t(($) => $.relations.save_error)); }
          }}><Unlink /></Button>
        </div>)}
      </div> : null}
      {error && <p role="alert" className="break-words text-body text-destructive">{error}</p>}
    </section>
  );
}
