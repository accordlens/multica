import { z } from "zod";

const instant = z.string().datetime({ offset: true });
const reference = z.string().regex(/^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,99}$/);
const percentage = z.number().min(0).max(100).nullable().default(null);
const timezone = z.string().min(1).refine((value) => {
  if (value === "Local") return false;
  try { new Intl.DateTimeFormat("en", { timeZone: value }); return true; }
  catch { return false; }
});

export const AccountUsageSnapshotSchema = z.object({
  version: z.literal(1),
  observation_id: reference,
  provider: z.literal("anthropic"),
  account_ref: reference,
  scope: z.literal("subscription_account"),
  availability: z.enum(["unknown", "available", "rate_limited"]),
  limits: z.array(z.object({
    id: reference,
    kind: z.literal("usage"),
    unit: z.literal("percent"),
    limit: percentage,
    used: percentage,
    remaining: percentage,
    window: z.object({
      kind: z.enum(["unknown", "rolling", "fixed"]),
      duration_seconds: z.number().int().positive().nullable().default(null),
      starts_at: instant.nullable().default(null),
      ends_at: instant.nullable().default(null),
    }),
    reset_at: instant.nullable().default(null),
    reset_timezone: timezone.nullable().default(null),
  }).refine((limit) => (limit.reset_at === null) === (limit.reset_timezone === null))).min(1).max(8)
    .refine((limits) => new Set(limits.map((limit) => limit.id)).size === limits.length),
  provenance: z.object({
    kind: z.literal("manual"),
    surface: z.enum(["claude_code_usage", "claude_settings_usage"]),
    evidence_ref: reference,
    recorded_by: z.string().optional(),
    recorded_at: instant.optional(),
  }),
  observed_at: instant,
  stale_after_seconds: z.number().int().min(1).max(86400),
});

export type AccountUsageSnapshot = z.infer<typeof AccountUsageSnapshotSchema>;

export const AccountUsageProjectionSchema = z.object({
  state: z.enum(["unknown", "fresh", "stale"]),
  reason: z.string(),
  as_of: instant.nullable(),
  owner_id: z.string(),
  runtime_ids: z.array(z.string()),
  snapshot: AccountUsageSnapshotSchema.nullable(),
}).refine((projection) => projection.state === "unknown" || projection.snapshot !== null);

export type AccountUsageProjection = z.infer<typeof AccountUsageProjectionSchema>;

export const EMPTY_ACCOUNT_USAGE: AccountUsageProjection = {
  state: "unknown", reason: "snapshot_unavailable", as_of: null,
  owner_id: "", runtime_ids: [], snapshot: null,
};

// Recompute freshness when the existing view renders or refetches. Passing a
// reset never changes the observed figures or makes a provider available.
export function accountUsageFreshness(projection: AccountUsageProjection, now: number) {
  const s = projection.snapshot;
  if (!s || projection.state === "unknown") return { state: "unknown", reason: projection.reason } as const;
  if (Date.parse(s.observed_at) > now) return { state: "unknown", reason: "snapshot_invalid" } as const;
  if (s.limits.some((limit) => limit.reset_at && Date.parse(limit.reset_at) <= now)) {
    return { state: "stale", reason: "reset_requires_observation" } as const;
  }
  if (s.limits.some((limit) => limit.window.ends_at && Date.parse(limit.window.ends_at) <= now)) {
    return { state: "stale", reason: "window_requires_observation" } as const;
  }
  if (Date.parse(s.observed_at) + s.stale_after_seconds * 1000 <= now) {
    return { state: "stale", reason: "observation_expired" } as const;
  }
  return { state: projection.state, reason: projection.reason };
}
