export type IssueRelationType = "blocked_by" | "blocks" | "related";
export type DependencyConditionKind = "done" | "in_review" | "accepted" | "run_completed";
export interface DependencyCondition {
  kind: DependencyConditionKind;
  run_id?: string;
  source_step?: string;
}
export interface IssueRelation {
  id: string;
  type: string;
  issue: { id: string; identifier: string; title: string; status: string; status_category?: string | null; revision: number };
  condition: { kind: string; run_id?: string; source_step?: string };
  source_revision: number;
  source_evidence_version: string;
  target_step: string;
  satisfied: boolean;
  reason: string;
  provenance: Record<string, unknown>;
  created_at: string;
  removed_at: string | null;
}
export interface IssueRelationsResponse {
  relations: IssueRelation[];
  ready: boolean;
  reason: string;
  step: string;
  available: boolean;
}
export interface CreateIssueRelation {
  issue_id: string;
  type: IssueRelationType;
  condition: DependencyCondition;
  target_step?: string;
}

export interface IssueProvenanceLink {
  id: string; kind: string; run_id: string; step: string; url?: string; commit_sha?: string;
  artifact_id?: string; sha256?: string; dirty_hash?: string; source: string;
  actor_type?: string; actor_id?: string; created_at: string; removed_at?: string; removed_by?: string;
}
export interface IssueProvenanceResponse {
  links: IssueProvenanceLink[];
  runs: { run_id: string; step: string; status: string; created_at: string; workflow_id: string; definition_sha256: string; workflow_evidence: unknown }[];
  artifacts: { id: string; filename: string; run_id: string; created_at: string }[];
  has_more: boolean;
  offset: number;
  available: boolean;
}
