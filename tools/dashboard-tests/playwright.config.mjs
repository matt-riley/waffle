import { defineConfig } from "@playwright/test";

const viewports = [
  { name: "desktop", viewport: { width: 1470, height: 1000 } },
  { name: "tablet", viewport: { width: 768, height: 1000 } },
  { name: "mobile", viewport: { width: 375, height: 812 } },
  { name: "narrow", viewport: { width: 320, height: 812 } },
];

export default defineConfig({
  testDir: "./tests",
  // Visual baselines are platform-neutral so one reviewed set gates every
  // platform (macOS local runs and the Linux CI gate). Regenerate with
  // --update-snapshots after a deliberate visual change (#469).
  snapshotPathTemplate: "{testDir}/{testFilePath}-snapshots/{arg}{ext}",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: process.env.CI ? [["line"], ["github"]] : "line",
  timeout: 30_000,
  expect: {
    timeout: 5_000,
  },
  use: {
    channel: "chrome",
    colorScheme: "light",
    reducedMotion: "no-preference",
    screenshot: "only-on-failure",
    trace: "off",
  },
  projects: viewports.map(({ name, viewport }) => ({
    name,
    use: { viewport },
  })),
});
