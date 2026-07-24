import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";

import { expect, test } from "@playwright/test";

const testsDir = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(testsDir, "../../..");
const canaries = [
  "desk-secret-canary",
  "WAFFLE_PRIVATE_ENV",
  "/var/lib/waffle/private",
  "mcp --raw-command-canary",
];

let server;
let baseURL;

test.describe.configure({ mode: "serial" });

test.beforeAll(async () => {
  ({ child: server, url: baseURL } = await startFixture());
});

test.afterAll(async () => {
  await stopFixture(server);
});

test("fixture serves the embedded Desk through the production security boundary", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the fixture contract once.");
  const response = await page.goto(deskURL("today"));

  expect(response?.status()).toBe(200);
  expect(response?.headers()["content-security-policy"]).toContain(
    "default-src 'self'",
  );
  await expect(page).toHaveTitle("Waffle Desk");
  await expect(page.locator(".today")).toBeVisible();
  await expectNoCanaries(page);
});

test("all five destinations render their production section", async ({ page }) => {
  const destinations = [
    ["today", ".today", "Release review"],
    ["tasks", ".tasks", "What Waffle is carrying"],
    ["workspaces", ".workspaces", "Where Waffle is working"],
    ["memory", ".memory", "Memory"],
    ["capabilities", "#desk-capabilities", "Models, skills, and connections"],
  ];

  for (const [section, root, heading] of destinations) {
    await page.goto(deskURL(section));
    const sectionRoot = page.locator(root);
    await expect(sectionRoot).toBeVisible();
    await expect(
      sectionRoot.getByRole("heading", { name: heading, exact: true }),
    ).toBeVisible();
    await expectNoHorizontalOverflow(page);
    await expectNoCanaries(page);
  }
});

test("session model remains scoped away from the Waffle-wide default", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the stateful scope flow once.");
  await page.goto(deskURL("today"));
  const sessionModel = page.getByLabel("Session model");
  await expect(sessionModel).toBeEnabled();
  await sessionModel.selectOption("local");
  await expect(sessionModel).toHaveValue("local");

  await page.reload();
  await expect(page.getByLabel("Session model")).toHaveValue("local");

  await page.getByRole("link", { name: "Capabilities", exact: true }).click();
  await expect(page).toHaveURL(/section=capabilities/);
  const globalDefault = page.locator("#capability-models .capability-card").filter({
    hasText: "primary",
  });
  await expect(globalDefault).toContainText("Waffle-wide default");
});

test("attention task opens its persisted session at Today", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the stateful handoff flow once.");
  await page.goto(deskURL("tasks"));
  await page.getByRole("button", { name: "Attention", exact: true }).click();
  const card = page.locator("[data-task-id='run-attention']");
  await expect(card).toContainText("failed");
  await card.getByRole("link", { name: "Open at Desk", exact: true }).click();

  await expect(page).toHaveURL(/section=today.*session_id=session-primary/);
  await expect(page.getByRole("heading", { name: "Release review", exact: true })).toBeVisible();
});

test("keyboard entry and dialog focus return remain usable", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the keyboard flow once.");
  await page.goto(deskURL("workspaces"));

  await page.keyboard.press("Tab");
  await expect(page.getByRole("link", { name: "Skip to main content" })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.locator("#main-content")).toBeFocused();

  const opener = page.getByRole("button", { name: "Open repository", exact: true });
  await opener.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByLabel("Repository", { exact: true })).toBeFocused();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(opener).toBeFocused();
});

test("reduced motion preserves an intelligible, overflow-free desk", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the motion preference flow once.");
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto(deskURL("capabilities"));
  await expect(page.getByText("Capabilities are current.", { exact: true })).toBeVisible();
  await expectNoHorizontalOverflow(page);
});

test("reviewed skill installation remains inactive", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the staged install flow once.");
  await page.goto(deskURL("capabilities"));
  await page.getByLabel("Allowed local path").fill("/allowed/fixture-reviewed");
  await page.getByRole("button", { name: "Stage review", exact: true }).click();

  const review = page.locator("#capability-skill-review");
  await expect(review).toBeVisible();
  await expect(review).toContainText("fixture-reviewed");
  await review.getByRole("button", { name: "Install inactive", exact: true }).click();

  await expect(page.getByText("Skill installed inactive.", { exact: true })).toBeVisible();
  const installed = page.locator("#capability-skills .capability-card").filter({
    hasText: "fixture-reviewed",
  });
  await expect(installed).toContainText("Installed inactive");
  await expect(installed.getByRole("button", { name: "Activate", exact: true })).toBeVisible();
});

test("provider enrollment clears and never renders its credential", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the credential boundary flow once.");
  const credential = "desk-secret-canary";
  await page.goto(deskURL("capabilities"));
  await page.getByLabel("Connection name").fill("secondary");
  await page.getByLabel("Provider type").fill("openai");
  await page.getByLabel("First model alias").fill("secondary");
  await page.getByLabel("Provider model ID").fill("fixture-secondary");
  await page.getByLabel("Credential").fill(credential);
  await page.getByRole("button", { name: "Enroll provider", exact: true }).click();

  await expect(page.getByText("Provider enrolled.", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Credential")).toHaveValue("");
  await expectNoCanaries(page);
});

test("200 percent zoom preserves keyboard-discoverable content", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the explicit zoom gate once.");
  await page.setViewportSize({ width: 735, height: 500 });
  await page.goto(deskURL("today"));
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Emulation.setPageScaleFactor", { pageScaleFactor: 2 });

  await expect(page.getByRole("link", { name: "Skip to main content" })).toBeAttached();
  await expect(page.getByRole("button", { name: "Send message", exact: true })).toBeVisible();
  await expectNoHorizontalOverflow(page);
});

function deskURL(section) {
  return `${baseURL}/desk/?section=${encodeURIComponent(section)}`;
}

async function expectNoHorizontalOverflow(page) {
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBe(true);
}

async function expectNoCanaries(page) {
  const text = await page.locator("body").innerText();
  for (const canary of canaries) {
    expect(text).not.toContain(canary);
  }
}

async function startFixture() {
  const child = spawn(
    "go",
    ["run", "./tools/dashboard-tests/fixtures/fake-server.go"],
    {
      cwd: repositoryRoot,
      env: {
        ...process.env,
        GOCACHE:
          process.env.GOCACHE || path.join(os.tmpdir(), "waffle-dashboard-go-build"),
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  let errors = "";
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk) => {
    errors += chunk;
  });

  const output = readline.createInterface({ input: child.stdout });
  const firstLine = await fixtureURL(child, output, () => errors);
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(firstLine)) {
    throw new Error(`unexpected dashboard fixture URL: ${firstLine}\n${errors}`);
  }
  child.fixtureProtocolError = "";
  output.on("line", (line) => {
    child.fixtureProtocolError += `unexpected fixture stdout: ${line}\n`;
  });
  return { child, url: firstLine };
}

async function fixtureURL(child, output, getErrors) {
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      cleanup();
      reject(new Error(`dashboard fixture timed out\n${getErrors()}`));
    }, 30_000);
    const onLine = (line) => {
      cleanup();
      resolve(line);
    };
    const onExit = (code, signal) => {
      cleanup();
      reject(
        new Error(
          `dashboard fixture exited before readiness (${code ?? signal})\n${getErrors()}`,
        ),
      );
    };
    const onError = (error) => {
      cleanup();
      reject(error);
    };
    const cleanup = () => {
      clearTimeout(timeout);
      output.off("line", onLine);
      child.off("exit", onExit);
      child.off("error", onError);
    };
    output.on("line", onLine);
    child.on("exit", onExit);
    child.on("error", onError);
  });
}

async function stopFixture(child) {
  if (!child || child.exitCode !== null) {
    return;
  }
  child.kill("SIGTERM");
  await Promise.race([
    new Promise((resolve) => child.once("exit", resolve)),
    new Promise((resolve) => setTimeout(resolve, 5_000)),
  ]);
  if (child.exitCode === null) {
    child.kill("SIGKILL");
  }
  if (child.fixtureProtocolError) {
    throw new Error(child.fixtureProtocolError);
  }
}
