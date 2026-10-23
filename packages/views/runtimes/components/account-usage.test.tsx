import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import { AccountUsageProjectionSchema, EMPTY_ACCOUNT_USAGE } from "@multica/core/runtimes/account-usage";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { AccountUsageDetails } from "./account-usage";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

function Wrapper({ children }: { children: ReactNode }) {
  return <I18nProvider locale="en" resources={TEST_RESOURCES}>{children}</I18nProvider>;
}

afterEach(() => { cleanup(); vi.useRealTimers(); });

describe("shared provider account display", () => {
  it("shows one pool, sourced figures, unknown and zero independently of run costs", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-10-23T08:05:00Z"));
    const snapshot = JSON.parse(readFileSync(resolve("../../server/pkg/accountusage/testdata/claude-manual.json"), "utf8"));
    snapshot.limits[0].remaining = 0;
    snapshot.availability = "rate_limited";
    const projection = AccountUsageProjectionSchema.parse({
      state: "fresh", reason: "manual_snapshot", as_of: "2026-10-23T08:05:00Z", owner_id: "owner",
      runtime_ids: ["runtime-a", "runtime-b"], snapshot,
    });
    render(<AccountUsageDetails projection={projection} />, { wrapper: Wrapper });
    expect(screen.getAllByRole("heading", { name: "Shared provider account" })).toHaveLength(1);
    expect(screen.getByText("0%")).toBeVisible();
    expect(screen.getByText("37%")).toBeVisible();
    expect(screen.getAllByText("UNKNOWN").length).toBeGreaterThan(0);
    expect(screen.getByText(/Europe\/Warsaw/)).toBeVisible();
    expect(screen.getByText(/Rate limited at observation/)).toBeVisible();
    expect(screen.getByText(/Shared by 2 visible runtimes/)).toBeVisible();
    expect(screen.getByText(/record a fresh snapshot before retrying/)).toBeVisible();
    expect(screen.getByText(/Recording a snapshot does not reset/)).toBeVisible();
  });

  it("makes a missing account explicit even when there is no run history", () => {
    render(<AccountUsageDetails projection={EMPTY_ACCOUNT_USAGE} />, { wrapper: Wrapper });
    expect(screen.getAllByText("UNKNOWN")).toHaveLength(2);
    expect(screen.queryByText("0%")).not.toBeInTheDocument();
  });
});
