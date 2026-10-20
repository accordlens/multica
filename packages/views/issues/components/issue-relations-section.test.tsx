// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { renderWithI18n } from "../../test/i18n";

const mocks = vi.hoisted(() => ({ list: vi.fn(), provenance: vi.fn(), create: vi.fn(), remove: vi.fn(), accept: vi.fn() }));
vi.mock("@multica/core/api", () => ({ api: {
  listIssueRelations: (id: string) => mocks.list(id), listIssueProvenance: () => mocks.provenance(),
  createIssueRelation: (...args: unknown[]) => mocks.create(...args),
  removeIssueRelation: (...args: unknown[]) => mocks.remove(...args),
  acceptIssueRelation: (...args: unknown[]) => mocks.accept(...args),
} }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace" }));
vi.mock("@multica/core/paths", () => ({ useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}` }) }));
vi.mock("../../navigation", () => ({ AppLink: ({ href, children }: { href: string; children: ReactNode }) => <a href={href}>{children}</a> }));
import { IssueRelationsSection } from "./issue-relations-section";

let qc: QueryClient;
beforeEach(() => {
  qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  mocks.provenance.mockResolvedValue({ available: true, links: [], artifacts: [], runs: [], has_more: false });
});
afterEach(() => { cleanup(); qc.clear(); vi.resetAllMocks(); });
const render = () => renderWithI18n(<QueryClientProvider client={qc}><IssueRelationsSection issueId="target" /></QueryClientProvider>);
const relation = { id: "edge", type: "blocked_by", issue: { id: "source", identifier: "MUL-2", title: "Infrastructure readiness", status: "in_review", revision: 7 }, condition: { kind: "accepted" }, source_revision:7,source_evidence_version:"a".repeat(64), satisfied: false, reason: "awaiting_human_acceptance", target_step: "review" };

it("shows the waiting reason and accepts exactly the displayed source revision", async () => {
  mocks.list.mockResolvedValue({ available: true, ready: false, step: "review", relations: [relation] });
  mocks.accept.mockResolvedValue(undefined);
  render();
  expect(await screen.findByText("Waiting for human acceptance")).toBeVisible();
  expect(screen.getByRole("link", { name: "MUL-2 Infrastructure readiness" })).toHaveAttribute("href", "/acme/issues/source");
  fireEvent.click(screen.getByRole("button", { name: "Accept revision 7" }));
  await waitFor(() => expect(mocks.accept).toHaveBeenCalledWith("target", "edge", 7, "a".repeat(64)));
});

it("keeps input and a concrete error when a cycle is refused", async () => {
  mocks.list.mockResolvedValue({ available: true, ready: true, step: "todo", relations: [] });
  mocks.create.mockRejectedValue(new Error("dependency cycle: MUL-1 → MUL-2 → MUL-1"));
  render();
  fireEvent.click(screen.getByRole("button", { name: "Add relation" }));
  fireEvent.change(screen.getByLabelText("Issue ID or key"), { target: { value: "MUL-2" } });
  fireEvent.submit(screen.getByRole("form", { name: "Add relation" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("MUL-1 → MUL-2 → MUL-1");
  expect(screen.getByLabelText("Issue ID or key")).toHaveValue("MUL-2");
});

it("does not claim readiness or render unsafe source URLs from degraded data", async () => {
  mocks.list.mockResolvedValue({ available: false, ready: false, relations: [] });
  mocks.provenance.mockResolvedValue({ available: true, links: [{ id: "link", run_id: "run", kind: "pull_request", url: "javascript:alert(1)", step: "test" }], artifacts: [] });
  render();
  expect(await screen.findByText("Could not load relations.")).toBeVisible();
  expect(screen.queryByText("Inputs satisfied")).toBeNull();
  expect(screen.queryByRole("link")).toBeNull();
});
