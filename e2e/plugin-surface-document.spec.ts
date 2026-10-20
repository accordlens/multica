import { expect, test } from "@playwright/test";
import { buildSurfaceFrameDocument } from "../packages/views/plugins/surface-document";

test.describe("hosted plugin surface document", () => {
  test("replacing the host-authored wrapper starts a fresh guest without hostile navigation", async ({ page }) => {
    await page.route("https://plugin-content.example.test/**", async (route) => {
      const challenge = new URL(route.request().url()).pathname.slice(1);
      await route.fulfill({
        contentType: "text/html",
        body: `<!doctype html><script>
          const channel = new MessageChannel();
          parent.postMessage({ type: "multica:plugin-bridge-connect", version: 2,
            challenge: ${JSON.stringify(challenge)} }, "*", [channel.port1]);
        </script>`,
      });
    });
    await page.setContent(`<script>
      window.surfaceState = { connected: 0, navigated: 0 };
      addEventListener("message", event => {
        if (event.data?.type === "multica:plugin-bridge-connect" && event.ports[0]) {
          window.surfaceState.connected++;
          event.ports[0].close();
        }
        if (event.data?.type === "multica:plugin-surface-navigated") window.surfaceState.navigated++;
      });
    </script><iframe id="surface" sandbox="allow-scripts allow-same-origin"></iframe>`);

    for (const [index, proof] of ["first", "second"].entries()) {
      const wrapper = buildSurfaceFrameDocument({
        url: `https://plugin-content.example.test/${proof}`,
        bridgeToken: proof,
      });
      await page.locator("#surface").evaluate((frame, srcdoc) => {
        (frame as HTMLIFrameElement).srcdoc = srcdoc;
      }, wrapper);
      await expect.poll(() => page.evaluate(() =>
        (window as unknown as { surfaceState: { connected: number } }).surfaceState.connected,
      )).toBe(index + 1);
    }
    expect(await page.evaluate(() =>
      (window as unknown as { surfaceState: { navigated: number } }).surfaceState.navigated,
    )).toBe(0);
  });

  test("relays a hosted guest's terminal error once", async ({ page }) => {
    await page.route("https://plugin-content.example.test/**", async (route) => {
      await route.fulfill({
        contentType: "text/html",
        body: `<!doctype html><script>
          parent.postMessage({ type: "multica:plugin-surface-error" }, "*");
          parent.postMessage({ type: "multica:plugin-surface-error" }, "*");
        </script>`,
      });
    });
    await page.setContent(`<script>
      window.surfaceErrors = 0;
      addEventListener("message", event => {
        if (event.data?.type === "multica:plugin-surface-error" &&
            event.source === document.querySelector("#surface").contentWindow) window.surfaceErrors++;
      });
    </script><iframe id="surface" sandbox="allow-scripts allow-same-origin"></iframe>`);
    await page.locator("#surface").evaluate((frame, srcdoc) => {
      (frame as HTMLIFrameElement).srcdoc = srcdoc;
    }, buildSurfaceFrameDocument({ url: "https://plugin-content.example.test/error", bridgeToken: "proof" }));
    await expect.poll(() => page.evaluate(() =>
      (window as unknown as { surfaceErrors: number }).surfaceErrors,
    )).toBe(1);
    await page.evaluate(() => new Promise<void>((done) => requestAnimationFrame(() => requestAnimationFrame(() => done()))));
    expect(await page.evaluate(() => (window as unknown as { surfaceErrors: number }).surfaceErrors)).toBe(1);
  });
});
