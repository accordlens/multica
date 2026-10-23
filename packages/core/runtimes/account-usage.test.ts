// @vitest-environment node
import { readFileSync } from "node:fs";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "../api/client";
import {
  AccountUsageSnapshotSchema, AccountUsageProjectionSchema, accountUsageFreshness,
  EMPTY_ACCOUNT_USAGE, type AccountUsageProjection,
} from "./account-usage";

function projection(): AccountUsageProjection {
  return AccountUsageProjectionSchema.parse({
    state: "fresh", reason: "manual_snapshot", as_of: "2026-10-23T08:05:00Z", owner_id: "owner",
    runtime_ids: ["runtime-a", "runtime-b"],
    snapshot: JSON.parse(readFileSync(new URL("../../../server/pkg/accountusage/testdata/claude-manual.json", import.meta.url), "utf8")),
  });
}

afterEach(() => vi.unstubAllGlobals());

describe("provider account usage contract", () => {
  it("keeps null amounts separate from zero and does not derive remaining from used", () => {
    const p = projection();
    expect(p.snapshot?.limits[0]?.used).toBe(37);
    expect(p.snapshot?.limits[0]?.remaining).toBeNull();
    p.snapshot!.limits[0]!.remaining = 0;
    expect(AccountUsageProjectionSchema.parse(p).snapshot?.limits[0]?.remaining).toBe(0);
    expect(p.snapshot?.limits[1]?.reset_at).toBeNull();
  });

  it("expires evidence at the age boundary and requires a new observation after reset", () => {
    const p = projection();
    expect(accountUsageFreshness(p, Date.parse("2026-10-23T08:14:59Z")).state).toBe("fresh");
    expect(accountUsageFreshness(p, Date.parse("2026-10-23T08:15:00Z"))).toEqual({ state: "stale", reason: "observation_expired" });
    expect(accountUsageFreshness(p, Date.parse("2026-10-23T10:00:00Z"))).toEqual({ state: "stale", reason: "reset_requires_observation" });
    expect(p.snapshot?.limits[0]?.remaining).toBeNull();
    expect(accountUsageFreshness(p, Date.parse("2026-10-23T07:00:00Z")).state).toBe("unknown");
  });

  it("preserves the reset instant and the source zone across a DST boundary", () => {
    const s = projection().snapshot!;
    s.limits[0]!.reset_at = "2026-10-25T02:30:00+01:00";
    const parsed = AccountUsageSnapshotSchema.parse(s);
    expect(Date.parse(parsed.limits[0]!.reset_at!)).toBe(Date.parse("2026-10-25T01:30:00Z"));
    expect(parsed.limits[0]?.reset_timezone).toBe("Europe/Warsaw");
    s.limits[0]!.reset_timezone = "Mars/Olympus";
    expect(AccountUsageSnapshotSchema.safeParse(s).success).toBe(false);
  });

  it("expires an ended fixed window even when reset is unknown", () => {
    const p = projection();
    p.snapshot!.limits[0]!.reset_at = null;
    p.snapshot!.limits[0]!.reset_timezone = null;
    p.snapshot!.limits[1]!.window.ends_at = "2026-10-23T08:06:00Z";
    expect(accountUsageFreshness(p, Date.parse("2026-10-23T08:06:00Z"))).toEqual({ state: "stale", reason: "window_requires_observation" });
    expect(p.snapshot?.limits[1]?.reset_at).toBeNull();
  });

  it("rejects unknown schemas, unsourced evidence and a reset without its zone", () => {
    const s = projection().snapshot!;
    expect(AccountUsageSnapshotSchema.safeParse({ ...s, version: 2 }).success).toBe(false);
    expect(AccountUsageSnapshotSchema.safeParse({ ...s, provenance: { ...s.provenance, kind: "model_estimate" } }).success).toBe(false);
    s.limits[0]!.reset_timezone = null;
    expect(AccountUsageSnapshotSchema.safeParse(s).success).toBe(false);
  });

  it("reads the separate API projection and degrades malformed figures to unknown", async () => {
    const valid = projection();
    const bad = projection();
    const malformed: unknown = { ...bad, snapshot: { ...bad.snapshot, limits: [{ ...bad.snapshot!.limits[0], remaining: "many" }] } };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify(valid)))
      .mockResolvedValueOnce(new Response(JSON.stringify(malformed)));
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");
    await expect(client.getRuntimeAccountUsage("runtime-a")).resolves.toEqual(valid);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("https://api.example.test/api/runtimes/runtime-a/usage/account");
    await expect(client.getRuntimeAccountUsage("runtime-a")).resolves.toEqual(EMPTY_ACCOUNT_USAGE);
  });
});
