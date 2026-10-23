"use client";

import { useId } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { runtimeAccountUsageOptions } from "@multica/core/runtimes/queries";
import { accountUsageFreshness, EMPTY_ACCOUNT_USAGE } from "@multica/core/runtimes/account-usage";
import { useT } from "../../i18n";

export function RuntimeAccountUsage({ runtimeId }: { runtimeId: string }) {
  const wsId = useWorkspaceId();
  const { data = EMPTY_ACCOUNT_USAGE, isError } = useQuery(runtimeAccountUsageOptions(wsId, runtimeId));
  return <AccountUsageDetails projection={isError ? EMPTY_ACCOUNT_USAGE : data} />;
}

export function AccountUsageDetails({ projection }: { projection: typeof EMPTY_ACCOUNT_USAGE }) {
  const titleId = useId();
  const { t, i18n } = useT("runtimes");
  const freshness = accountUsageFreshness(projection, Date.now());
  const snapshot = freshness.state === "unknown" ? null : projection.snapshot;
  const locale = i18n.resolvedLanguage ?? i18n.language;
  const percentage = (value: number | null) => value === null ? t(($) => $.account_usage.unknown) : `${value}%`;
  const date = (value: string, zone: string) => new Intl.DateTimeFormat(locale, {
    dateStyle: "medium", timeStyle: "short", timeZone: zone,
  }).format(new Date(value));
  const duration = (seconds: number) => {
    const units = [[86400, "day"], [3600, "hour"], [60, "minute"], [1, "second"]] as const;
    const [divisor, unit] = units.find(([size]) => seconds % size === 0)!;
    return new Intl.NumberFormat(locale, { style: "unit", unit, unitDisplay: "short" }).format(seconds / divisor);
  };

  return (
    <section aria-labelledby={titleId} className="space-y-3 rounded-xl border bg-card p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 id={titleId} className="text-body font-medium">{t(($) => $.account_usage.title)}</h3>
        <span className="text-caption text-muted-foreground">{t(($) => $.account_usage[freshness.state])}</span>
      </div>
      {snapshot ? (
        <>
          <p className="break-words text-body-sm">{snapshot.provider} / {snapshot.account_ref}</p>
          <p className="text-caption text-muted-foreground">
            {t(($) => $.account_usage[`availability_${snapshot.availability}`])} · {t(($) => $.account_usage.shared, { count: projection.runtime_ids.length })}
          </p>
          <ul className="space-y-3">
            {snapshot.limits.map((limit) => (
              <li key={limit.id} className="space-y-1">
                <p className="break-words text-body-sm font-medium">{limit.id}</p>
                <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-caption sm:grid-cols-3">
                  <div><dt className="text-muted-foreground">{t(($) => $.account_usage.total)}</dt><dd>{percentage(limit.limit)}</dd></div>
                  <div><dt className="text-muted-foreground">{t(($) => $.account_usage.used)}</dt><dd>{percentage(limit.used)}</dd></div>
                  <div><dt className="text-muted-foreground">{t(($) => $.account_usage.remaining)}</dt><dd>{percentage(limit.remaining)}</dd></div>
                  <div><dt className="text-muted-foreground">{t(($) => $.account_usage.window)}</dt><dd>
                    {t(($) => $.account_usage[`window_${limit.window.kind}`])}
                    {limit.window.duration_seconds !== null && ` (${duration(limit.window.duration_seconds)})`}
                    {(limit.window.starts_at || limit.window.ends_at) && (
                      <span className="block">{`${limit.window.starts_at ? date(limit.window.starts_at, "UTC") : t(($) => $.account_usage.unknown)} – ${limit.window.ends_at ? date(limit.window.ends_at, "UTC") : t(($) => $.account_usage.unknown)} (UTC)`}</span>
                    )}
                  </dd></div>
                  <div className="col-span-2"><dt className="text-muted-foreground">{t(($) => $.account_usage.reset)}</dt><dd>
                    {limit.reset_at && limit.reset_timezone ? `${date(limit.reset_at, limit.reset_timezone)} (${limit.reset_timezone})` : t(($) => $.account_usage.unknown)}
                  </dd></div>
                </dl>
              </li>
            ))}
          </ul>
          <p className="text-caption text-muted-foreground">
            {t(($) => $.account_usage.observed)}: {`${date(snapshot.observed_at, "UTC")} (UTC)`}
            {" · "}{t(($) => $.account_usage.source)}: {t(($) => $.account_usage[`source_${snapshot.provenance.surface}`])}
            {" · "}{snapshot.provenance.evidence_ref}
          </p>
        </>
      ) : <p className="text-body-sm">{t(($) => $.account_usage.unknown)}</p>}
      {(!snapshot || freshness.state === "stale" || snapshot.availability !== "available") && (
        <p className="text-body-sm">{t(($) => $.account_usage.refresh)}</p>
      )}
      <p className="text-caption text-muted-foreground">{t(($) => $.account_usage.notice)}</p>
    </section>
  );
}
