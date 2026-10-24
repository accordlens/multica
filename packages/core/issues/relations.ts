import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { CreateIssueRelation } from "../types/issue-relation";

export const issueRelationKeys = {
  all: (wsId: string) => ["issue-relations", wsId] as const,
  detail: (wsId: string, issueId: string) => [...issueRelationKeys.all(wsId), issueId] as const,
};

export function issueRelationsOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: issueRelationKeys.detail(wsId, issueId),
    queryFn: () => api.listIssueRelations(issueId),
    enabled: !!wsId && !!issueId,
    refetchOnWindowFocus: true,
  });
}

export function useCreateIssueRelation(wsId: string, issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateIssueRelation) => api.createIssueRelation(issueId, body),
    onSettled: () => qc.invalidateQueries({ queryKey: issueRelationKeys.all(wsId) }),
  });
}

export function useRemoveIssueRelation(wsId: string, issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (relationId: string) => api.removeIssueRelation(issueId, relationId),
    onSettled: () => qc.invalidateQueries({ queryKey: issueRelationKeys.all(wsId) }),
  });
}

export function useAcceptIssueRelation(wsId: string, issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, revision, evidenceVersion }: { id: string; revision: number; evidenceVersion: string }) => api.acceptIssueRelation(issueId, id, revision, evidenceVersion),
    onSettled: () => qc.invalidateQueries({ queryKey: issueRelationKeys.all(wsId) }),
  });
}

export function issueProvenanceOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: [...issueRelationKeys.detail(wsId, issueId), "provenance"],
    queryFn: () => api.listIssueProvenance(issueId),
    enabled: !!wsId && !!issueId,
    refetchOnWindowFocus: true,
  });
}
export function useRemoveIssueProvenance(wsId: string, issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, runId }: { id: string; runId: string }) => api.removeIssueProvenance(issueId, runId, id),
    onSettled: () => qc.invalidateQueries({ queryKey: issueRelationKeys.all(wsId) }),
  });
}
