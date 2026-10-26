import { defineConfig } from "@playwright/test";
import base from "./playwright.config";

export default defineConfig(base, {
  testMatch: ["issue-relations.spec.ts", "workflow-script-evidence.spec.ts"],
  use: { ...base.use, video: "on", trace: "on", screenshot: "on" },
});
