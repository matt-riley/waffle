import { defineConfig } from "@playwright/test";

const viewports = [
  { name: "desktop", viewport: { width: 1470, height: 1000 } },
  { name: "tablet", viewport: { width: 768, height: 1000 } },
  { name: "mobile", viewport: { width: 375, height: 812 } },
  { name: "narrow", viewport: { width: 320, height: 812 } },
];

export default defineConfig({
  testDir: "./tests",
  // Visual baselines are platform-specific (font metrics differ across
  // macOS/Linux), so the platform is part of the snapshot name. Tests skip
  // when no baseline exists for the current platform and are enforced where
  // one does (#469). Regenerate with --update-snapshots.
  snapshotPathTemplate: "{testDir}/{testFilePath}-snapshots/{arg}-{platform}{ext}",
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
