// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createStore } from "zustand/vanilla";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import {
  type IssueViewState,
  viewStoreSlice,
} from "@multica/core/issues/stores/view-store";
import {
  useViewStore,
  ViewStoreProvider,
} from "@multica/core/issues/stores/view-store-context";
import { renderWithI18n } from "../../test/i18n";
import { IssueFilterMenu } from "./issues-header";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

function renderFilterMenu() {
  setApiInstance({
    listIssueStatuses: async () => ({ statuses: [], categories: [], total: 0 }),
    listProperties: async () => ({ properties: [] }),
  } as unknown as ApiClient);
  const store = createStore<IssueViewState>()(viewStoreSlice);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  function Harness() {
    const dateFilter = useViewStore((state) => state.dateFilter);
    return (
      <IssueFilterMenu
        trigger={<button type="button">Filter</button>}
        dateFilter={dateFilter}
        onDateFilterChange={store.getState().setDateFilter}
      />
    );
  }
  renderWithI18n(
    <QueryClientProvider client={queryClient}>
      <ViewStoreProvider store={store}>
        <Harness />
      </ViewStoreProvider>
    </QueryClientProvider>,
  );
  return store;
}

afterEach(() => {
  cleanup();
  document.body.innerHTML = "";
  vi.restoreAllMocks();
});

describe("IssueFilterMenu date selection with real menu primitives", () => {
  it.each([false, true])(
    "retains Today when keyboard-selecting Updated after an explicit close: %s",
    async (closeFirst) => {
      const store = renderFilterMenu();
      const user = userEvent.setup();
      const trigger = screen.getByRole("button", { name: "Filter" });
      await user.click(trigger);
      let dateItem = await screen.findByRole("menuitem", { name: /^Date/ });
      act(() => dateItem.focus());
      await user.keyboard("{ArrowRight}");
      await user.click(await screen.findByRole("menuitem", { name: "Today" }));

      const preset = store.getState().dateFilter;
      expect(preset).toMatchObject({ field: "created_at" });
      expect(preset?.from).toEqual(preset?.to);
      // Today deliberately keeps this mixed field/preset menu open.
      expect(trigger).toHaveAttribute("aria-expanded", "true");
      expect(screen.getByRole("menuitemradio", { name: "Created" })).toBeChecked();
      if (closeFirst) {
        await user.keyboard("{Escape}{Escape}");
        expect(trigger).toHaveAttribute("aria-expanded", "false");
      }

      if (trigger.getAttribute("aria-expanded") === "false") {
        await user.click(trigger);
      }
      dateItem = await screen.findByRole("menuitem", { name: /^Date/ });
      act(() => dateItem.focus());
      await user.keyboard("{ArrowRight}");
      const updated = await screen.findByRole("menuitemradio", { name: "Updated" });
      act(() => updated.focus());
      await user.keyboard("{Enter}");

      expect(updated).toBeChecked();
      expect(screen.getByRole("menuitemradio", { name: "Created" })).not.toBeChecked();
      expect(store.getState().dateFilter).toEqual({ ...preset, field: "updated_at" });
    },
  );
});
