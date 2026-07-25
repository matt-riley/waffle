import { spawn } from "node:child_process";
import http from "node:http";
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
  "desk-github-key-canary",
  "github.example.invalid",
  "desk-intake-token-canary",
  "telegram:canary",
];

let server;
let baseURL;

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({}, testInfo) => {
  testInfo.setTimeout(120_000);
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

test("security boundary rejects cross-site requests and protects mutations", async ({ request }) => {
  test.skip(test.info().project.name !== "desktop", "Run the security contract once.");

  const allowed = await request.get(deskURL("today"));
  expect(allowed.status()).toBe(200);
  expect(allowed.headers()).toMatchObject({
    "content-security-policy":
      "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
    "referrer-policy": "no-referrer",
    "x-content-type-options": "nosniff",
    "x-frame-options": "DENY",
  });
  expect(allowed.headers()["access-control-allow-origin"]).toBeUndefined();

  for (const headers of [
    { Host: "attacker.example" },
    { Origin: "https://attacker.example" },
    { "Sec-Fetch-Site": "cross-site" },
  ]) {
    const rejected = await rawRequest(baseURL, "/desk/", { headers });
    expect(rejected.status).toBe(403);
    expect(rejected.headers["access-control-allow-origin"]).toBeUndefined();
  }

  const missingToken = await rawRequest(baseURL, "/api/v1/desk/chat/open", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Idempotency-Key": crypto.randomUUID(),
    },
    body: JSON.stringify({ continue: true }),
  });
  expect(missingToken.status).toBe(403);

  const bootstrap = await request.get(`${baseURL}/api/v1/desk/bootstrap`);
  expect(bootstrap.status()).toBe(200);
  const { request_token: requestToken } = await bootstrap.json();
  const missingIdempotency = await rawRequest(
    baseURL,
    "/api/v1/desk/chat/open",
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Waffle-Desk-Token": requestToken,
      },
      body: JSON.stringify({ continue: true }),
    },
  );
  expect(missingIdempotency.status).toBe(400);
  expect(missingIdempotency.body).toContain("idempotency_key_required");
});

test("connections expose only allowlisted fields from canary-bearing config", async ({ request }) => {
  test.skip(test.info().project.name !== "desktop", "Run the redaction contract once.");

  const response = await request.get(`${baseURL}/api/v1/desk/connections`);
  expect(response.status()).toBe(200);
  const raw = await response.text();
  const records = JSON.parse(raw);
  expect(records).toEqual([
    {
      kind: "provider",
      name: "fixture",
      status: "configured",
    },
    {
      kind: "mcp",
      name: "fixture-tools",
      status: "configured",
    },
    {
      egress: "restricted",
      guidance: "Runs in a sandbox.",
      kind: "profile",
      name: "reviewer",
      profile: "reviewer",
      sandbox_mode: "docker",
      status: "configured",
    },
    {
      guidance:
        "Workspace git auth is brokered; containers never hold a credential.",
      kind: "github",
      name: "github",
      status: "configured",
    },
    {
      concurrency: 2,
      guidance:
        "Issues matching this label are picked up by the issue profile.",
      kind: "intake",
      label: "waffle",
      name: "fixture/board",
      status: "configured",
    },
  ]);
  expectNoCanariesIn(raw);
  // The app and installation IDs are credentials-adjacent identifiers and
  // must not appear even though they are not strings in config (#182 AC2).
  for (const identifier of ["4242", "8484"]) {
    expect(raw).not.toContain(identifier);
  }
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

test("Today sends a streamed reply and confirms cancellation", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the stateful chat flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  const message = page.getByLabel("Message Waffle");
  await message.fill("Summarize the fixture");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".user-message .message-body")).toHaveText(
    "Summarize the fixture",
  );
  await expect(page.locator(".waffle-message .message-body")).toHaveText(
    "Fixture reply",
  );
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  await message.fill("Wait until I cancel");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const cancel = page.getByRole("button", { name: "Cancel turn", exact: true });
  await expect(cancel).toBeEnabled();
  await cancel.click();
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await expect(cancel).toBeDisabled();
});

test("Today renders Markdown, keyboard send, and paired tool evidence", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the rich transcript flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  const message = page.getByLabel("Message Waffle");
  await message.fill("Show markdown");
  await message.press("Control+Enter");

  const reply = page.locator(".waffle-message .message-body");
  await expect(reply.getByRole("heading", { name: "Fixture markdown" })).toBeVisible();
  await expect(reply.locator("li")).toHaveCount(2);
  await expect(reply.locator("pre code")).toContainText('fmt.Println("fixture")');
  await expect(reply.locator("code")).toContainText(["mise", 'fmt.Println("fixture")']);
  await expect(reply.getByRole("button", { name: "Copy" })).toBeVisible();

  const tool = page.locator("#desk-tool-activity .activity-row");
  await expect(tool).toHaveCount(1);
  await expect(tool).toContainText("fixture_read · 18 ms · succeeded");
  await expect(tool).toHaveClass(/is-success/);
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
});

test("Today exposes existing commands and resumes a recent session in place", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the command surface once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "New conversation", exact: true }).click();
  await expect(page.locator("#desk-session-title")).toHaveText("Fresh conversation");
  await expect(page.locator("#desk-transcript")).toContainText("Fresh start");

  await page.getByRole("button", { name: "Recent conversations", exact: true }).click();
  await page.getByRole("button", { name: /Release review ·/ }).click();
  await expect(page.locator("#desk-session-title")).toHaveText("Release review");

  for (const [summary, button, result] of [
    ["Usage", "Load usage", /3 requests · 120 in · 45 out · 10 reserved/],
    ["Permissions", "Load permissions", /Sandbox: workspace-write/],
    ["Working set", "Load working set", /Verify the Today experience/],
    ["Commands", "Load commands", /\/new · Start a conversation/],
  ]) {
    const panel = page.locator(".context-panels details").filter({ hasText: summary });
    await panel.locator("summary").click();
    await panel.getByRole("button", { name: button, exact: true }).click();
    await expect(panel.locator(".context-panel-result")).toContainText(result);
  }
  await expect(page.locator("#desk-sandbox")).toHaveText("workspace-write");
});

test("Today reload and navigate-away recovery returns to a usable single desk", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the ownership lifecycle once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const before = await page.evaluate(() =>
    JSON.parse(sessionStorage.getItem("waffle.desk.today.owner.v1")),
  );
  expect(before.client_id).toBeTruthy();
  expect(before.reattach_token).toBeTruthy();

  await page.reload();
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await expect(page.getByLabel("Message Waffle")).toBeEnabled();
  const after = await page.evaluate(() =>
    JSON.parse(sessionStorage.getItem("waffle.desk.today.owner.v1")),
  );
  expect(after.reattach_token).not.toBe(before.reattach_token);

  await page.getByRole("link", { name: "Tasks", exact: true }).click();
  await expect(page).toHaveURL(/section=tasks/);
  await page.getByRole("link", { name: "Today", exact: true }).click();
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const message = page.getByLabel("Message Waffle");
  await message.fill("Usable after navigation");
  await message.press("Control+Enter");
  await expect(page.locator(".waffle-message .message-body")).toContainText("Fixture reply");
});

test("Today reconnects after SSE drop without tearing down the desk", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the recovery flow once.");

  // Gate the event stream so we can force a drop and then restore it without
  // racing Playwright unroute against an exponential backoff timer that was
  // scheduled while the route was still aborting every attempt.
  let allowEvents = false;
  await page.route("**/api/v1/desk/events?*", async (route) => {
    if (!allowEvents) {
      await route.abort("connectionrefused");
      return;
    }
    await route.continue();
  });

  await page.goto(deskURL("today"));

  // Dropped stream surfaces a reconnecting state rather than full teardown.
  await expect(page.locator("#desk-phase")).toHaveText("Reconnecting");
  await expect(page.locator("#desk-stale-status")).toBeHidden();
  // Composer stays live while reconnecting (recoverable path).
  await expect(page.getByLabel("Message Waffle")).toBeEnabled();

  // Restore the event stream; the client auto-reconnects from the stored
  // cursor without requiring "Refresh Desk". First retry is immediate, so
  // recovery should land well inside this window.
  allowEvents = true;
  await expect(page.locator("#desk-phase")).toHaveText("Ready", { timeout: 20_000 });
  await expect(page.locator("#desk-stale-status")).toBeHidden();
  await expect(page.getByLabel("Message Waffle")).toBeEnabled();
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
  await expect(card).toContainText("Run needs attention");
  await card.getByRole("link", { name: "Open at Desk", exact: true }).click();

  await expect(page).toHaveURL(/section=today.*session_id=session-primary/);
  await expect(page.getByRole("heading", { name: "Release review", exact: true })).toBeVisible();
});

test("workspace lifecycle is deterministic and dirty close remains blocked", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the workspace lifecycle once.");
  await page.goto(deskURL("workspaces"));

  const cards = page.locator(".workspace-card");
  await expect(cards).toHaveCount(2);
  await expect
    .poll(() => cards.evaluateAll((items) => items.map((item) => item.dataset.workspaceId)))
    .toEqual(["workspace-clean", "workspace-dirty"]);

  const dirty = page.locator("[data-workspace-id='workspace-dirty']");
  // Git state is readable on the card itself, without opening the close flow.
  await expect(dirty.locator(".workspace-git")).toContainText("feature/dirty");
  await expect(dirty.locator(".workspace-git")).toContainText("1 uncommitted file");
  await expect(dirty.locator(".workspace-git")).toContainText("1 ahead · 0 behind");
  await expect(dirty.locator(".workspace-git")).toContainText("abc1234 local commit");
  await expect(
    page.locator("[data-workspace-id='workspace-clean'] .workspace-git"),
  ).toContainText("Clean");

  const dirtyReview = dirty.getByRole("button", { name: "Review close", exact: true });
  await dirtyReview.click();
  const closeDialog = page.locator("#workspace-close-dialog");
  await expect(closeDialog).toBeVisible();
  await expect(page.locator("#workspace-close-dirty")).toHaveText("M main.go");
  await expect(page.locator("#workspace-close-unpushed")).toHaveText(
    "abc123 local commit",
  );
  await expect(
    closeDialog.getByRole("button", { name: "Close workspace", exact: true }),
  ).toBeDisabled();
  await closeDialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dirtyReview).toBeFocused();

  let clean = page.locator("[data-workspace-id='workspace-clean']");
  await clean.getByRole("button", { name: "Idle", exact: true }).click();
  clean = page.locator("[data-workspace-id='workspace-clean']");
  await expect(clean).toHaveAttribute("data-status", "idle");
  await clean.getByRole("button", { name: "Resume", exact: true }).click();
  clean = page.locator("[data-workspace-id='workspace-clean']");
  await expect(clean).toHaveAttribute("data-status", "open");

  await page.getByRole("button", { name: "Open repository", exact: true }).click();
  await page.getByLabel("Repository", { exact: true }).fill("matt-riley/new-repo");
  await page.getByLabel("Profile").fill("reviewer");
  await page.getByRole("button", { name: "Open workspace", exact: true }).click();
  let opened = page.locator("[data-workspace-id='workspace-opened']");
  await expect(opened).toContainText("matt-riley/new-repo");

  await opened.getByRole("button", { name: "Open at Desk", exact: true }).click();
  await expect(page).toHaveURL(/section=today.*session_id=session-primary/);
  await page.goto(deskURL("workspaces"));

  opened = page.locator("[data-workspace-id='workspace-opened']");
  await opened.getByRole("button", { name: "Review close", exact: true }).click();
  await expect(closeDialog).toBeVisible();
  await expect(page.locator("#workspace-close-dirty")).toHaveText("Clean");
  await expect(page.locator("#workspace-close-unpushed")).toHaveText("None");
  await closeDialog.getByRole("button", { name: "Close workspace", exact: true }).click();
  await expect(page.locator("[data-workspace-id='workspace-opened']")).toHaveAttribute(
    "data-status",
    "closed",
  );
});

test("memory search attaches one source and forgets only after confirmation", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the memory lifecycle once.");
  await page.goto(deskURL("memory"));

  await page.getByLabel("Search turns, summaries, and notes").fill("release artifact");
  await page.getByRole("button", { name: "Search memory", exact: true }).click();
  const note = page.locator(".memory-hit").filter({ hasText: "a1b2c3" });
  await expect(note).toContainText("Use the verified release artifact.");

  await page.getByLabel("Session ID").fill("session-primary");
  await note.getByRole("button", { name: "Attach to session", exact: true }).click();
  await expect(page.locator("#memory-attach-status")).toHaveText(
    "Memory reference attached to the session.",
  );

  const forget = note.getByRole("button", { name: "Forget…", exact: true });
  await forget.click();
  const dialog = page.locator("#memory-forget-dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Affects Waffle-owned memory only.");
  await expect(dialog).toContainText("Does not erase provider logs.");
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(note).toBeVisible();

  await forget.click();
  await dialog.getByRole("button", { name: "Forget note", exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(note).toHaveCount(0);
  await expect(page.locator("#memory-results")).toContainText(
    "Reviewing the release queue.",
  );
});

test("keyboard navigation reaches every destination and dialog returns focus", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the keyboard flow once.");
  const destinations = [
    ["Today", "today", ".today"],
    ["Tasks", "tasks", ".tasks"],
    ["Workspaces", "workspaces", ".workspaces"],
    ["Memory", "memory", ".memory"],
    ["Capabilities", "capabilities", "#desk-capabilities"],
  ];
  for (const [name, section, root] of destinations) {
    await page.goto(deskURL("today"));
    const skip = page.getByRole("link", { name: "Skip to main content" });
    await skip.focus();
    await page.keyboard.press("Enter");
    await expect(page.locator("#main-content")).toBeFocused();
    const link = page.getByRole("link", { name, exact: true });
    await link.focus();
    await expect(link).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`section=${section}`));
    await expect(page.locator(root)).toBeVisible();
  }

  await page.goto(deskURL("workspaces"));
  const opener = page.getByRole("button", { name: "Open repository", exact: true });
  await opener.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByLabel("Repository", { exact: true })).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.getByLabel("Profile")).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(opener).toBeFocused();
});

test("reduced motion suppresses animation and preserves an overflow-free desk", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the motion preference flow once.");
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.getByLabel("Message Waffle").fill("Check reduced motion");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const message = page.locator(".message").first();
  await expect(message).toBeVisible();
  const motion = await message.evaluate((element) => {
    const style = getComputedStyle(element);
    return {
      animationDuration: style.animationDuration,
      animationIterations: style.animationIterationCount,
      transitionDuration: style.transitionDuration,
    };
  });
  expect(parseFloat(motion.animationDuration)).toBeLessThanOrEqual(0.00001);
  expect(motion.animationIterations).toBe("1");
  expect(parseFloat(motion.transitionDuration)).toBeLessThanOrEqual(0.00001);
  await expectNoHorizontalOverflow(page);
});

test("skill installation stays inactive until explicit activation", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the staged install flow once.");
  await page.goto(deskURL("capabilities"));
  await page.getByLabel("Allowed local path").fill("/allowed/fixture-reviewed");
  await page.getByRole("button", { name: "Stage review", exact: true }).click();

  const review = page.locator("#capability-skill-review");
  await expect(review).toBeVisible();
  await expect(review).toContainText("fixture-reviewed");
  await review.getByRole("button", { name: "Install inactive", exact: true }).click();

  const installed = page.locator("#capability-skills .capability-card").filter({
    hasText: "fixture-reviewed",
  });
  await expect(installed).toContainText("Installed inactive");
  const activation = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/skills/fixture-reviewed/activate") &&
      response.status() === 202,
  );
  await installed.getByRole("button", { name: "Activate", exact: true }).click();
  await activation;
  await expect(page.locator("#capability-restart-status")).toBeVisible();
  await page.reload();
  await expect(page.getByText("Capabilities are current.", { exact: true })).toBeVisible();
  await expect(installed).toContainText("Active");
  await expect(
    installed.getByRole("button", { name: "Activate", exact: true }),
  ).toHaveCount(0);
});

test("provider enrollment clears and never renders its credential", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the credential boundary flow once.");
  const credential = "desk-secret-canary";
  await page.goto(deskURL("capabilities"));
  const providerForm = page.locator("#capability-provider-form");
  await providerForm.getByLabel("Connection name").fill("secondary");
  await providerForm.getByLabel("Provider type").fill("openai");
  await providerForm.getByLabel("First model alias").fill("secondary");
  await providerForm.getByLabel("Provider model ID").fill("fixture-secondary");
  await providerForm.getByLabel("Credential").fill(credential);
  const enrollment = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/providers") &&
      response.status() === 202,
  );
  await providerForm.getByRole("button", { name: "Enroll provider", exact: true }).click();

  await enrollment;
  await expect(page.getByText("Capabilities are current.", { exact: true })).toBeVisible();
  await expect(providerForm.getByLabel("Credential")).toHaveValue("");
  await expect(
    page.locator("#capability-models .capability-card").filter({ hasText: "secondary" }),
  ).toContainText("fixture-secondary");
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
  expectNoCanariesIn(text);
  expectNoCanariesIn(await page.content());
  const storage = await page.evaluate(() => ({
    local: { ...localStorage },
    session: { ...sessionStorage },
  }));
  expectNoCanariesIn(JSON.stringify(storage));
}

function expectNoCanariesIn(value) {
  for (const canary of canaries) {
    expect(value).not.toContain(canary);
  }
}

async function rawRequest(base, pathname, options = {}) {
  const url = new URL(pathname, base);
  const body = options.body || "";
  return new Promise((resolve, reject) => {
    const request = http.request(
      {
        hostname: url.hostname,
        port: url.port,
        path: url.pathname + url.search,
        method: options.method || "GET",
        headers: {
          Host: url.host,
          ...(body ? { "Content-Length": Buffer.byteLength(body) } : {}),
          ...options.headers,
        },
      },
      (response) => {
        let responseBody = "";
        response.setEncoding("utf8");
        response.on("data", (chunk) => {
          responseBody += chunk;
        });
        response.on("end", () => {
          resolve({
            status: response.statusCode,
            headers: response.headers,
            body: responseBody,
          });
        });
      },
    );
    request.on("error", reject);
    request.end(body);
  });
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
      detached: true,
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
    }, 120_000);
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
  const gracefulExit = waitForFixtureExit(child, 5_000);
  signalFixture(child, "SIGTERM");
  if (!(await gracefulExit)) {
    const forcedExit = waitForFixtureExit(child, 5_000);
    signalFixture(child, "SIGKILL");
    if (!(await forcedExit)) {
      throw new Error("dashboard fixture did not exit after SIGKILL");
    }
  }
  if (child.fixtureProtocolError) {
    throw new Error(child.fixtureProtocolError);
  }
}

function signalFixture(child, signal) {
  try {
    process.kill(-child.pid, signal);
  } catch {
    child.kill(signal);
  }
}

async function waitForFixtureExit(child, timeout) {
  if (child.exitCode !== null) {
    return true;
  }
  return new Promise((resolve) => {
    const onExit = () => {
      cleanup();
      resolve(true);
    };
    const timer = setTimeout(() => {
      cleanup();
      resolve(false);
    }, timeout);
    const cleanup = () => {
      clearTimeout(timer);
      child.off("exit", onExit);
    };
    child.on("exit", onExit);
  });
}
