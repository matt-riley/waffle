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

test("posture shows the prompt, each policy tier, and the rule behind a refusal", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the posture surface once.");
  await page.goto(deskURL("today"));

  const dialog = page.locator("#desk-posture-dialog");
  await expect(dialog).toBeHidden();
  await page.locator("#desk-posture-open").click();
  await expect(dialog).toBeVisible();

  // AC1: the effective prompt, with its source labelled.
  await expect(dialog).toContainText("Inline in config.toml");
  await expect(dialog).toContainText("You review changes.");

  // AC2: the tiers are shown as layers, not one flattened list.
  for (const tier of [
    "Agent group",
    "Profile narrowing",
    "Repo policy (WAFFLE.md)",
    "Effective",
  ]) {
    await expect(dialog).toContainText(tier);
  }
  await expect(dialog.locator("[data-layer='profile']")).toContainText("git push");

  // AC3: a refusal names the rule that produced it.
  await expect(dialog).toContainText("no-force-push");
  await expect(dialog).toContainText("git push --force");

  // AC4: read-only, and the host path in the group's deny prefixes is withheld
  // rather than rendered.
  await expect(dialog.locator("form")).toHaveCount(0);
  await expectNoCanaries(page);

  await dialog.getByRole("button", { name: "Close", exact: true }).click();
  await expect(dialog).toBeHidden();
});

test("setup reports each prerequisite and routes to the control that fixes it", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the bootstrap surface once.");
  await page.goto(deskURL("capabilities"));

  const checklist = page.locator("#setup-checklist");
  await expect(checklist).toBeVisible();

  // AC1: every prerequisite is named with its state. The fixture install has
  // an identity and a default model outstanding, and a provider already
  // enrolled, so all three states are on screen at once.
  await expect(checklist.locator(".setup-step")).toHaveCount(5);
  await expect(checklist.locator("[data-step='provider']")).toHaveAttribute(
    "data-state",
    "configured",
  );
  for (const step of ["identity", "models", "profile"]) {
    await expect(checklist.locator(`[data-step='${step}']`)).toHaveAttribute(
      "data-state",
      "missing",
    );
  }

  // AC2: a prerequisite Desk cannot satisfy states the exact command instead
  // of offering a button that could not work.
  const dashboardStep = checklist.locator("[data-step='dashboard']");
  await expect(dashboardStep).toContainText("waffle setup");
  await expect(dashboardStep.locator("button")).toHaveCount(0);

  // AC2: the actions route to the existing controls rather than standing up a
  // second form — in particular a second credential channel.
  await checklist
    .locator("[data-step='models']")
    .getByRole("button", { name: "Set the default model", exact: true })
    .click();
  await expect(page.locator("#capability-default-alias")).toBeFocused();

  await openCapabilityTab(page, "Setup");
  await checklist
    .locator("[data-step='profile']")
    .getByRole("button", { name: "Create a starter profile", exact: true })
    .click();
  await expect(page.locator("#profile-name")).toHaveValue("main");
  await expect(page.locator("#profile-system")).not.toHaveValue("");

  await openCapabilityTab(page, "Setup");

  // AC4: creating the identity is a guarded mutation that returns no key
  // material, and the step only flips because the server says it did.
  const creation = page.waitForResponse((response) =>
    response.url().endsWith("/api/v1/desk/setup/identity"),
  );
  await checklist
    .locator("[data-step='identity']")
    .getByRole("button", { name: "Create identity", exact: true })
    .click();
  const created = await creation;
  expect(created.status()).toBe(202);
  expect(await created.text()).not.toContain("AGE-SECRET-KEY");
  await expect(checklist.locator("[data-step='identity']")).toHaveAttribute(
    "data-state",
    "configured",
  );

  await expectNoHorizontalOverflow(page);
  await expectNoCanaries(page);
});

test("Today points a partially configured install at the checklist", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the bootstrap banner once.");
  await page.goto(deskURL("today"));
  const banner = page.locator("#desk-setup-banner");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("Waffle is not set up yet.");
  await banner.getByRole("link", { name: "Finish setup", exact: true }).click();
  await expect(page.locator("#setup-checklist")).toBeVisible();
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

test("form-and-list sections swap real embedded htmx fragments", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the htmx fragment contract once.");

  const cases = [
    ["capabilities", "/api/v1/desk/capabilities?part=models", "#capability-models"],
    ["tasks", "/api/v1/desk/tasks?filter=all", "#tasks-list"],
    ["workspaces", "/api/v1/desk/workspaces", "#workspaces-list"],
  ];
  for (const [section, route, target] of cases) {
    const fragment = page.waitForResponse(
      (response) =>
        response.url().includes(route) &&
        response.request().headers()["hx-request"] === "true",
    );
    await page.goto(deskURL(section));
    const response = await fragment;
    expect(response.status()).toBe(200);
    expect(response.headers()["content-type"]).toContain("text/html");
    await expect(page.locator(target)).toHaveAttribute("data-waffle-fragment", "true");
  }

  await page.goto(deskURL("memory"));
  await page.getByLabel("Search turns, summaries, and notes").fill("release artifact");
  const memoryFragment = page.waitForResponse(
    (response) =>
      response.url().includes("/api/v1/desk/memory") &&
      response.request().headers()["hx-request"] === "true",
  );
  await page.getByRole("button", { name: "Search memory", exact: true }).click();
  const memoryResponse = await memoryFragment;
  expect(memoryResponse.status()).toBe(200);
  expect(memoryResponse.headers()["content-type"]).toContain("text/html");
  await expect(page.locator("#memory-results")).toHaveAttribute("data-waffle-fragment", "true");
});

test("Tasks htmx schedule form creates, edits, and reports filter state", async ({ page }) => {
  await page.goto(deskURL("tasks"));
  await page.getByRole("button", { name: "New schedule", exact: true }).click();
  const form = page.locator("#task-schedule-form");
  await expect(form.getByRole("button", { name: "Cancel", exact: true })).toBeVisible();
  await form.getByLabel("Name").fill("Invalid fixture schedule");
  await form.getByLabel("Cron schedule").fill("not-a-cron");
  await form.getByLabel("Prompt").fill("This must not be saved");
  const invalid = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/tasks/schedules") &&
      response.request().method() === "POST" &&
      response.status() === 422,
  );
  await form.getByRole("button", { name: "Create schedule", exact: true }).click();
  await invalid;
  await expect(form.locator("[data-waffle-error='true']")).toContainText("schedule definition is invalid");

  await form.getByLabel("Name").fill("Fixture schedule");
  await form.getByLabel("Cron schedule").fill("0 10 * * 1-5");
  await form.getByLabel("Prompt").fill("Review the fixture queue");
  const created = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/tasks/schedules") &&
      response.request().method() === "POST" &&
      response.status() === 201,
  );
  await form.getByRole("button", { name: "Create schedule", exact: true }).click();
  await created;

  const card = page.locator("[data-task-id='job-added']");
  await expect(card).toContainText("Fixture schedule");
  await card.getByRole("button", { name: "Edit schedule", exact: true }).click();
  await expect(form.getByLabel("Name")).toHaveValue("Fixture schedule");
  await expect(form.getByLabel("Cron schedule")).toHaveValue("0 10 * * 1-5");
  await form.getByLabel("Prompt").fill("Edited fixture queue");
  const updated = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/tasks/schedules/job-added") &&
      response.request().method() === "POST" &&
      response.status() === 200,
  );
  await form.getByRole("button", { name: "Save schedule", exact: true }).click();
  await updated;
  await expect(page.locator("[data-task-id='job-added']")).toContainText("Fixture schedule");

  await page.getByRole("button", { name: "Scheduled", exact: true }).click();
  await expect(page.locator("#task-filter-scheduled")).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator("#task-filter-all")).toHaveAttribute("aria-pressed", "false");
});

test("Tasks attention chip settles to a truthful count instead of Checking forever", async ({ page }) => {
  await page.goto(deskURL("tasks"));
  // The list fragment replaces the loading label with the settled count.
  await expect(page.locator("#tasks-attention-count")).toHaveText("1 task needs attention", {
    timeout: 10_000,
  });
  await expect(page.locator("#tasks-attention-count")).not.toHaveText("Checking attention");
  // The settled chip keeps its live styling (not the error treatment).
  await expect(page.locator("#tasks-attention-count")).not.toHaveClass(/is-error/);

  // Filtering reloads the fragment and keeps the count truthful.
  await page.getByRole("button", { name: "Attention", exact: true }).click();
  await expect(page.locator("#task-filter-attention")).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator("#tasks-attention-count")).toHaveText("1 task needs attention");
});

test("Capabilities htmx catalogue add, search, and prospective test use fragments", async ({ page }) => {
  await page.goto(deskURL("capabilities"));
  await openCapabilityTab(page, "Tools & connections");
  await openCapabilityDisclosure(page, "Enroll a provider");

  const providerForm = page.locator("#capability-provider-form");
  await providerForm.getByLabel("Connection name").fill("fixture");
  await providerForm.getByLabel("First model alias").fill("primary");
  await providerForm.getByLabel("Provider model ID").fill("primary-model");
  await providerForm.getByLabel("Credential").fill("fixture-test-credential");
  const testResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/providers/test") &&
      response.request().method() === "POST" &&
      response.status() === 200,
  );
  await page.getByRole("button", { name: "Test connection", exact: true }).click();
  await testResponse;
  await expect(page.locator("#capability-provider-status")).toContainText("Connection test succeeded.");
  await expect(providerForm.getByLabel("Credential")).toHaveValue("");

  await openCapabilityTab(page, "Models");
  await openCapabilityDisclosure(page, "Browse a provider catalogue");
  const catalogue = page.locator("#capability-catalogue-form");
  await catalogue.getByLabel("Enrolled connection").selectOption("fixture");
  const refreshed = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/models/catalogue/refresh") &&
      response.request().method() === "POST" &&
      response.status() === 200,
  );
  await catalogue.getByRole("button", { name: "Refresh catalogue", exact: true }).click();
  await refreshed;
  const results = page.locator("#capability-catalogue-results");
  await expect(results).toContainText("Fixture model");
  await expect(results.getByRole("button", { name: "Add as alias", exact: true })).toBeVisible();
  await results.getByLabel("Alias").fill("fixture-catalogue");
  const added = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/models") &&
      response.request().method() === "POST" &&
      response.status() === 202,
  );
  await results.getByRole("button", { name: "Add as alias", exact: true }).click();
  await added;
  await openCapabilityDisclosure(page, "Add a model");
  await expect(page.locator("#capability-model-form #capability-model-status")).toContainText("Capability change accepted.");
  await expect(results.getByRole("button", { name: "Enrolled", exact: true })).toBeDisabled();

  await page.locator("#capability-catalogue-search").fill("does-not-match");
  await expect(results.locator(".catalogue-card")).toBeHidden();
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

test("Today replaces the previous transcript when starting a new conversation", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the stateful chat flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  // Existing session with a completed turn.
  const message = page.getByLabel("Message Waffle");
  await message.fill("Summarize the fixture");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".user-message .message-body")).toHaveText(
    "Summarize the fixture",
  );
  await expect(page.locator(".waffle-message .message-body")).toHaveText(
    "Fixture reply",
  );

  // New conversation atomically replaces the old transcript with the new
  // session's empty state instead of leaving the prior turns behind (#455).
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "New conversation", exact: true }).click();
  await expect(page.locator("#desk-session-title")).toHaveText("Fresh conversation");
  await expect(page.locator("#desk-transcript .user-message")).toHaveCount(0);
  await expect(page.locator("#desk-transcript")).toContainText(
    "The desk is ready. What are we working on?",
  );

  // The first turn renders only into the new session's DOM.
  await message.fill("First message in the fresh session");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator("#desk-transcript .user-message .message-body")).toHaveText(
    "First message in the fresh session",
  );
  await expect(page.locator("#desk-transcript .waffle-message .message-body")).toHaveText(
    "Fixture reply",
  );
  await expect(page.locator("#desk-transcript .user-message")).toHaveCount(1);
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

  const table = reply.locator("table");
  await expect(table).toBeVisible();
  await expect(table.locator("thead th")).toHaveCount(2);
  await expect(table.locator("tbody tr")).toHaveCount(2);
  await expect(table.locator("td").first()).toHaveText("mise");

  const tool = page.locator("#desk-transcript .tool-chip");
  await expect(tool).toHaveCount(1);
  await expect(tool).toContainText("fixture_read");
  await expect(tool).toContainText("18 ms");
  await expect(tool).toHaveClass(/is-success/);
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
});


test("busy composer queues a visible follow-up that is held on cancel", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the queue flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  const message = page.getByLabel("Message Waffle");
  await message.fill("Wait until I cancel");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator("#desk-send")).toHaveText("Queue follow-up");
  await expect(message).toBeEnabled();

  await message.fill("queued follow-up");
  await page.keyboard.press("Enter");
  const banner = page.locator("#desk-queue");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("queued follow-up");
  await expect(page.locator("#desk-composer-status")).toContainText("Follow-up queued");

  await page.getByRole("button", { name: "Cancel turn", exact: true }).click();
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await expect(banner).toContainText("held for review");
  await expect(page.locator("#desk-send")).toHaveText("Send message");
});

test("completed turns edit and regenerate through safe branches", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the branch flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  const message = page.getByLabel("Message Waffle");
  await message.fill("Show markdown");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const reply = page.locator(".waffle-message .message-body");
  await expect(reply.locator("table")).toBeVisible();

  // The completed turn pair exposes Edit and Regenerate.
  const edit = page.getByRole("button", { name: "Edit and continue" });
  const regenerate = page.getByRole("button", { name: "Regenerate response" });
  await expect(edit).toBeVisible();
  await expect(regenerate).toBeVisible();

  // Regenerate branches and re-sends the prompt in the new branch.
  await regenerate.click();
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await expect(page.locator(".user-message .message-body").last()).toHaveText("Show markdown");
  await expect(reply.last()).toContainText("Fixture markdown");
  await expect(page.locator("#desk-composer-status")).toContainText(/branch/i);

  // Edit prefills the exact prompt and says the next send creates a branch.
  await page.getByRole("button", { name: "Edit and continue" }).last().click();
  await expect(message).toHaveValue("Show markdown");
  await expect(page.locator("#desk-composer-status")).toContainText(/branch/i);
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
});

test("wide markdown tables scroll inside the response without page overflow", async ({ page }) => {
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.getByLabel("Message Waffle").fill("wide table");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const table = page.locator(".waffle-message table");
  await expect(table).toBeVisible();
  await expect(table.locator("thead th")).toHaveCount(6);
  const scroll = page.locator(".table-scroll");
  await expect(scroll).toBeVisible();
  await expect(scroll).toHaveAttribute("aria-label", "Table");
  await expectNoHorizontalOverflow(page);
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
});

test("Today branches a conversation from a completed exchange", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the branch flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  // Produce one completed exchange so its message carries the branch action.
  const message = page.getByLabel("Message Waffle");
  await message.fill("Summarize the fixture");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".waffle-message .message-body")).toHaveText(
    "Fixture reply",
  );
  const branch = page.getByRole("button", {
    name: "Branch from the end of this conversation",
  });
  await expect(branch).toBeVisible();
  await branch.focus();
  await page.keyboard.press("Enter");

  await expect(page.locator("#desk-session-title")).toHaveText(
    "Branched conversation",
  );
  await expect(page.locator("#desk-fork")).toHaveText(
    "Branched from session session-primary at turn 2",
  );
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
});

test("Today attaches project context from the open workspace in place", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the project context flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  const panel = page.locator(".context-panels details").filter({ hasText: "Project context" });
  await panel.locator("summary").click();
  await panel.getByRole("button", { name: "Load project context", exact: true }).click();
  await expect(panel.locator(".context-panel-result")).toContainText(
    "No pinned resources",
  );

  // Pin a workspace file through the guarded mutation.
  await panel.getByLabel("Pin workspace file").fill("docs/plan.md");
  await panel.getByRole("button", { name: "Pin file", exact: true }).click();
  await expect(panel.locator(".project-resource-label")).toContainText("plan.md");

  // Attach it to the conversation; the panel flips to Detach.
  const fileRow = panel.locator(".project-resource").filter({ hasText: "plan.md" });
  await fileRow.getByRole("button", { name: "Attach", exact: true }).click();
  await expect(
    panel.locator(".project-resource").filter({ hasText: "plan.md" }),
  ).toContainText("Detach");

  // Add an owner note and attach it too.
  await panel.getByLabel("Add owner note").fill("Guidance");
  await panel.getByPlaceholder("Note body…").fill("Follow the release checklist.");
  await panel.getByRole("button", { name: "Add note", exact: true }).click();
  const noteRow = panel.locator(".project-resource").filter({ hasText: "Guidance" });
  await expect(noteRow).toContainText("note");
  await noteRow.getByRole("button", { name: "Attach", exact: true }).click();
  await expect(
    panel.locator(".project-resource").filter({ hasText: "Guidance" }),
  ).toContainText("Detach");
});



test("Today previews, downloads, and references a declared session artifact", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the artifact card flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);

  const message = page.getByLabel("Message Waffle");
  await message.fill("Make an artifact");
  await page.getByRole("button", { name: "Send message", exact: true }).click();

  const card = page.locator(".artifact-card");
  await expect(card).toBeVisible();
  await expect(card.locator(".artifact-name")).toHaveText("release.md");
  await expect(card.locator(".artifact-meta")).toContainText("text/markdown");

  await card.getByRole("button", { name: "Preview", exact: true }).click();
  await expect(card.locator(".artifact-preview-body")).toContainText(
    "Ready for review",
  );

  const download = page.waitForEvent("download");
  await card.getByRole("button", { name: "Download", exact: true }).click();
  const artifactDownload = await download;
  expect(artifactDownload.suggestedFilename()).toBe("release.md");

  await card.getByRole("button", { name: "Copy reference", exact: true }).click();
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
});



test("Today renders a source drawer with safe destinations after a cited reply", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the source drawer flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  const message = page.getByLabel("Message Waffle");
  await message.fill("Show sources");
  await message.press("Control+Enter");

  const reply = page.locator(".waffle-message").last();
  await expect(reply.locator(".message-body")).toContainText(
    "The release queue is summarized in the fixture sources.",
  );
  const drawer = reply.locator(".sources-drawer");
  await expect(drawer).toBeVisible();
  await expect(drawer.locator("summary")).toHaveText("Sources (2)");
  await drawer.locator("summary").click();
  const items = drawer.locator(".source-item");
  await expect(items).toHaveCount(2);
  const web = items.filter({ hasText: "Waffle fixture docs" });
  await expect(web.locator(".source-open")).toHaveAttribute(
    "href",
    "https://example.com/docs",
  );
  await expect(web.locator(".source-open")).toHaveAttribute("rel", "noopener noreferrer");
  const workspace = items.filter({ hasText: "Fixture plan" });
  await expect(workspace.locator(".source-open")).toHaveCount(0);
  await expect(workspace.locator(".source-kind")).toHaveText("Workspace source");
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
});

test("Today exposes existing commands and resumes a recent session in place", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the command surface once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "New conversation", exact: true }).click();
  await expect(page.locator("#desk-session-title")).toHaveText("Fresh conversation");
  // A new session owns an empty transcript: the previous conversation must
  // be replaced, not left behind (#455).
  await expect(page.locator("#desk-transcript")).toContainText(
    "The desk is ready. What are we working on?",
  );

  await page.getByRole("button", { name: "Recent conversations", exact: true }).click();
  await expect(page.getByRole("option", { name: /Release review/ })).toBeVisible();
  await page.getByRole("option", { name: /Release review/ }).click();
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

test("conversation rows rename, pin, and delete with a named confirmation", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the manage flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  // Create a disposable conversation to manage, independent of test order.
  page.once("dialog", async (dialog) => {
    await dialog.accept();
  });
  await page.getByRole("button", { name: "New conversation", exact: true }).click();
  await expect(page.locator("#desk-session-title")).toHaveText("Fresh conversation");
  await page.getByRole("button", { name: "Recent conversations", exact: true }).click();
  const freshChoice = page.getByRole("option", { name: /Fresh conversation/ });
  await expect(freshChoice).toBeVisible();

  const trigger = page.getByRole("button", { name: "Actions for Fresh conversation" });
  await trigger.click();
  const popover = trigger.locator("..").locator(".session-menu-popover");
  await expect(popover).toBeVisible();
  await expect(popover).toContainText("Rename");
  await expect(popover).toContainText("Pin");
  await expect(popover).toContainText("Delete");

  // Inline rename persists into the list.
  await popover.getByRole("menuitem", { name: "Rename", exact: true }).click();
  await page.getByLabel("Conversation title").fill("Fresh conversation v2");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("option", { name: /Fresh conversation v2/ })).toBeVisible();

  // Pin moves the row ahead with a Pinned label.
  const triggerV2 = page.getByRole("button", { name: "Actions for Fresh conversation v2" });
  await triggerV2.click();
  const popoverV2 = triggerV2.locator("..").locator(".session-menu-popover");
  await popoverV2.getByRole("menuitem", { name: "Pin", exact: true }).click();
  await expect(page.getByRole("option", { name: /Pinned/ })).toBeVisible();

  // Delete names the conversation in the confirmation before mutating.
  let dialogText = "";
  page.once("dialog", async (dialog) => {
    dialogText = dialog.message();
    await dialog.accept();
  });
  await triggerV2.click();
  await triggerV2.locator("..").locator(".session-menu-popover").getByRole("menuitem", { name: "Delete", exact: true }).click();
  await expect.poll(() => dialogText).toMatch(/Fresh conversation v2/);
  await expect(page.getByRole("option", { name: /Fresh conversation v2/ })).toHaveCount(0);
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
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

test("an active-session ownership conflict recovers inline instead of the fatal stale screen", async ({ page, browser }) => {
  test.skip(test.info().project.name !== "desktop", "Run the ownership recovery once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  // Lock the latest session as if another surface holds it, then open the
  // Desk from a second browser surface with no stored owner (#454).
  await page.request.post(`${baseURL}/api/v1/desk/test/lock-latest`);
  try {
    const context = await browser.newContext();
    try {
      const second = await context.newPage();
      await second.goto(deskURL("today"));

      // Bounded retries fail, then inline recovery appears; the fatal
      // out-of-date treatment is reserved for genuinely incompatible state.
      await expect(second.locator("#desk-phase")).toHaveText("Conversation in use", {
        timeout: 15_000,
      });
      await expect(second.locator("#desk-stale-label")).toHaveText(
        "This conversation is in use.",
      );
      await expect(second.locator("#desk-stale-message")).toContainText(
        "Another surface",
      );
      await expect(second.getByRole("button", { name: "Start a new conversation" })).toBeVisible();

      // Inline recovery opens a fresh session and returns the composer to a
      // usable state.
      await second.getByRole("button", { name: "Start a new conversation" }).click();
      await expect(second.locator("#desk-phase")).toHaveText("Ready");
      const message = second.getByLabel("Message Waffle");
      await expect(message).toBeEnabled();
      await message.fill("Usable after recovery");
      await second.getByRole("button", { name: "Send message", exact: true }).click();
      await expect(second.locator(".waffle-message .message-body")).toHaveText(
        "Fixture reply",
      );
    } finally {
      await context.close();
    }
  } finally {
    await page.request.post(`${baseURL}/api/v1/desk/test/lock-latest?on=0`);
  }
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
  await openCapabilityTab(page, "Models");
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

test("workspace cards lead with truthful metadata and distinct actions", async ({ page }) => {
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto(deskURL("workspaces"));
  // Page-level summary counts the rendered workspaces by status.
  await expect(page.locator("#workspaces-summary")).not.toBeEmpty({ timeout: 10_000 });
  await expect(page.locator("#workspaces-summary")).toContainText("open");

  const clean = page.locator("[data-workspace-id='workspace-clean']");
  await expect(clean).toBeVisible();
  // Metadata is ordered and truthful: profile and active session are shown,
  // opaque IDs stay secondary and copyable.
  await expect(clean.locator(".waffle-fragment-facts")).toContainText("Profile");
  await expect(clean.locator(".waffle-fragment-facts")).toContainText("reviewer");
  await expect(clean.locator(".waffle-fragment-facts")).toContainText("session-primary");
  const copySession = clean.getByRole("button", { name: "Copy session ID", exact: true });
  await expect(copySession).toBeVisible();
  await copySession.click();
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe("session-primary");
  // The transient feedback appears on the same control without a page change.
  await expect(clean.locator("[data-waffle-copy]")).toHaveText("Copied");

  // The primary continuation action is distinct from the destructive close.
  await expect(
    clean.getByRole("button", { name: "Open at Desk", exact: true }),
  ).toHaveClass(/workspace-primary/);
  await expect(
    clean.getByRole("button", { name: "Review close", exact: true }),
  ).toHaveClass(/workspace-danger-action/);
});

test("async review dialogs open as native modals with contained focus", async ({ page }) => {
  await page.goto(deskURL("workspaces"));
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const dirty = page.locator("[data-workspace-id='workspace-dirty']");
  await expect(dirty).toBeVisible();
  const review = dirty.getByRole("button", { name: "Review close", exact: true });
  await review.click();
  const dialog = page.locator("#workspace-close-dialog");
  await expect(dialog).toBeVisible();
  // The swapped fragment enters the modal top layer (backdrop included)
  // instead of the old pre-opened non-modal state (#457).
  expect(await dialog.evaluate((element) => element.matches(":modal"))).toBe(true);
  // Initial focus lands on Cancel, never the destructive confirmation.
  await expect(dialog.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
  // Tab and Shift+Tab stay inside the open dialog.
  await page.keyboard.press("Tab");
  expect(await dialog.evaluate((element) => element.contains(document.activeElement))).toBe(true);
  await page.keyboard.press("Shift+Tab");
  expect(await dialog.evaluate((element) => element.contains(document.activeElement))).toBe(true);
  // Escape closes and restores focus to the invoking control.
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(review).toBeFocused();
  // The pre-opened showModal InvalidStateError never fires.
  expect(errors).toEqual([]);
});

test("posture dialog contains keyboard focus and restores the opener", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the posture focus flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const trigger = page.getByRole("button", { name: "View system prompt and policy" });
  await trigger.click();
  const dialog = page.locator("#desk-posture-dialog");
  await expect(dialog).toBeVisible();
  await expect(page.locator("#desk-posture-close")).toBeFocused();
  for (let index = 0; index < 6; index += 1) {
    await page.keyboard.press("Tab");
    expect(
      await dialog.evaluate((element) => element.contains(document.activeElement)),
    ).toBe(true);
  }
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(trigger).toBeFocused();
});

test("memory attach uses a session picker with stale-selection recovery", async ({ page }) => {
  await page.goto(deskURL("memory"));
  const picker = page.locator("#memory-session");
  // The picker loads persisted sessions with human-readable labels.
  await expect(picker).toBeVisible();
  await expect(picker.locator("option")).toContainText(["Select a conversation", "Release review"]);
  await expect(picker.locator("option[value='session-primary']")).toContainText(/Release review/);

  // Attach stays disabled until a valid session is selected.
  const query = page.getByLabel("Search turns, summaries, and notes");
  await query.fill("release artifact");
  await page.getByRole("button", { name: "Search memory", exact: true }).click();
  const note = page.locator(".memory-hit").filter({ hasText: "a1b2c3" });
  const attach = note.getByRole("button", { name: "Attach to session", exact: true });
  await expect(attach).toBeDisabled();

  await picker.selectOption("session-primary");
  await expect(attach).toBeEnabled();
  await attach.click();
  await expect(page.locator("#memory-attach-status")).toHaveText(
    "Memory reference attached to the session.",
  );

  // A stale/deleted selection recovers: the attach fails and the picker
  // drops the invalid option instead of leaving it resubmittable.
  await page.request.post(`${baseURL}/api/v1/desk/test/memory-sessions?empty=1`);
  try {
    await picker.selectOption("session-primary");
    await attach.click();
    await expect(page.locator("#memory-attach-status")).toContainText(
      "target session was not found",
    );
    await expect(picker.locator("option[value='session-primary']")).toHaveCount(0);
    await expect(picker.locator("option")).toContainText("No persisted conversations yet");
    await expect(page.locator("#memory-session-empty")).toContainText("Start one in Today");
  } finally {
    await page.request.post(`${baseURL}/api/v1/desk/test/memory-sessions`);
  }
});

test("memory search status settles and never coexists with stale instructions", async ({ page }) => {
  await page.goto(deskURL("memory"));
  const status = page.locator("#memory-status");
  await expect(status).toHaveText("Enter a search to begin.");

  const query = page.getByLabel("Search turns, summaries, and notes");
  const search = page.getByRole("button", { name: "Search memory", exact: true });

  // Settled results: the count replaces the initial instruction.
  await query.fill("release artifact");
  await search.click();
  await expect(status).toHaveText("2 results");
  const note = page.locator(".memory-hit").filter({ hasText: "a1b2c3" });
  await expect(note.locator(".waffle-fragment-kind")).toHaveText("Note");
  await expect(note.locator(".waffle-fragment-excerpt")).toContainText(
    "Use the verified release artifact.",
  );
  // Metadata makes source/time/session scannable on the card.
  await expect(note.locator(".waffle-fragment-facts")).toContainText("Source ID");
  await expect(note.locator(".waffle-fragment-facts")).toContainText("Time");

  // No results is a distinct settled state, not the loading instruction.
  await page.request.post(`${baseURL}/api/v1/desk/test/memory-search?hits=0`);
  try {
    await query.fill("nothing matches this");
    await search.click();
    await expect(status).toHaveText("No attributed memory matched that search.");
    await expect(page.locator("#memory-results .waffle-fragment-empty")).toContainText(
      "No attributed memory",
    );
  } finally {
    await page.request.post(`${baseURL}/api/v1/desk/test/memory-search`);
  }

  // Total failure renders a distinct actionable state.
  await page.request.post(`${baseURL}/api/v1/desk/test/memory-search?error=all`);
  try {
    await query.fill("release artifact");
    await search.click();
    await expect(status).toHaveText("Memory search is unavailable right now.");
    await expect(page.locator("#memory-results .waffle-fragment-empty")).toContainText(
      "could not be completed",
    );
  } finally {
    await page.request.post(`${baseURL}/api/v1/desk/test/memory-search`);
  }

  // Partial failure keeps healthy results and names the limitation.
  await page.request.post(`${baseURL}/api/v1/desk/test/memory-search?error=notes`);
  try {
    await query.fill("release artifact");
    await search.click();
    await expect(status).toHaveText(
      "1 result(s) — some memory sources are unavailable.",
    );
  } finally {
    await page.request.post(`${baseURL}/api/v1/desk/test/memory-search`);
  }
});

test("memory search attaches one source and forgets only after confirmation", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the memory lifecycle once.");
  await page.goto(deskURL("memory"));

  await page.getByLabel("Search turns, summaries, and notes").fill("release artifact");
  await page.getByRole("button", { name: "Search memory", exact: true }).click();
  const note = page.locator(".memory-hit").filter({ hasText: "a1b2c3" });
  await expect(note).toContainText("Use the verified release artifact.");

  const picker = page.locator("#memory-session");
  await expect(picker.locator("option[value='session-primary']")).toContainText(/Release review/);
  await picker.selectOption("session-primary");
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
    // Wait for Today open to settle so async composer autofocus cannot race
    // the skip-link focus assertion under CI latency.
    await expect(page.locator("#desk-phase")).toHaveText("Ready");
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

test("fixed mobile navigation never obscures the composer, last content, or dialogs", async ({ page }) => {
  test.skip(
    !["tablet", "mobile", "narrow"].includes(test.info().project.name),
    "Run the obstruction checks on mobile widths.",
  );
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const message = page.getByLabel("Message Waffle");
  await message.fill("Check composer clearance");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".waffle-message .message-body")).toHaveText("Fixture reply");

  const nav = page.locator(".desk-navigation");
  const navBox = await nav.boundingBox();
  // The composer and its actions sit fully above the fixed navigation.
  await message.focus();
  await page.evaluate(() =>
    document.querySelector("#desk-message").scrollIntoView({ block: "end" }),
  );
  await page.evaluate(() =>
    document.querySelector("#desk-message").scrollIntoView({ block: "end" }),
  );
  const composer = await message.boundingBox();
  expect(composer.y + composer.height).toBeLessThanOrEqual(navBox.y + 0.5);

  // The last turn scrolls completely into view above the bar.
  const last = page.locator(".waffle-message").last();
  await last.scrollIntoViewIfNeeded();
  const lastBox = await last.boundingBox();
  expect(lastBox.y + lastBox.height).toBeLessThanOrEqual(navBox.y + 0.5);

  // Navigation labels stay legible (never sub-10px) at the narrowest width.
  const labelSize = await page
    .locator(".section-links a")
    .first()
    .evaluate((element) => parseFloat(getComputedStyle(element).fontSize));
  expect(labelSize).toBeGreaterThanOrEqual(10);

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
  await openCapabilityTab(page, "Skills");
  await openCapabilityDisclosure(page, "Add a skill for review");
  await page.getByLabel("Local skill path").fill("/allowed/fixture-reviewed");
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
  await openCapabilityTab(page, "Skills");
  await expect(installed).toContainText("Active");
  await expect(
    installed.getByRole("button", { name: "Activate", exact: true }),
  ).toHaveCount(0);
});

test("connection cards lead with real health and operator language", async ({ page }) => {
  await page.goto(deskURL("capabilities"));
  await openCapabilityTab(page, "Tools & connections");
  const card = page.locator(".connection-card").filter({ hasText: "fixture" });
  await expect(card).toBeVisible();
  // Unchecked state: never probed, provider default limit, no endpoint leak.
  await expect(card.locator(".waffle-fragment-kind")).toHaveText("Unchecked");
  await expect(card).toContainText("Provider default");
  await expect(card).toContainText("Last checkNever");
  await expect(card).toContainText("OpenAI-compatible driver");
  await expect(card).not.toContainText("11434");

  // A healthy probe updates the card in place.
  const checkButton = card.getByRole("button", { name: "Check connection", exact: true });
  await checkButton.click();
  await expect(card.locator(".waffle-fragment-kind")).toHaveText("Healthy");
  await expect(card).toContainText("Just now");
  await expect(card).not.toContainText("Never");

  // A failed probe renders a distinct failure state with the safe next step.
  await page.request.post(`${baseURL}/api/v1/desk/test/provider-probe?failure=unreachable`);
  try {
    await card.getByRole("button", { name: "Check connection", exact: true }).click();
    await expect(card.locator(".waffle-fragment-kind")).toHaveText("Failed");
    await expect(card).toContainText("Connection test could not reach the endpoint.");
    await expect(card).not.toContainText("11434");
  } finally {
    await page.request.post(`${baseURL}/api/v1/desk/test/provider-probe`);
  }
});

test("skill-import disclosure is removed when imports are disabled", async ({ page }) => {
  await page.goto(deskURL("capabilities"));
  await openCapabilityTab(page, "Skills");
  const disclosure = page.locator("#capability-skill-import-disclosure");
  // Imports are enabled in the fixture: the disclosure is interactive.
  await expect(disclosure).toBeVisible();
  await expect(disclosure).toHaveAttribute("aria-disabled", "false");

  await page.request.post(`${baseURL}/api/v1/desk/test/skill-imports?on=0`);
  try {
    await page.reload();
    // The disclosure is removed, the prerequisite names the safe next step,
    // and no interactive blank panel can be opened (#464).
    await expect(disclosure).toBeHidden();
    await expect(page.locator("#capability-skill-stage-prerequisite")).toBeVisible();
    await expect(page.locator("#capability-skill-stage-prerequisite")).toContainText(
      "Skill imports are disabled",
    );
    await expect(page.locator("#capability-skill-stage-form")).toBeHidden();
  } finally {
    await page.request.post(`${baseURL}/api/v1/desk/test/skill-imports`);
  }
});

test("provider enrollment clears and never renders its credential", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the credential boundary flow once.");
  const credential = "desk-secret-canary";
  await page.goto(deskURL("capabilities"));
  await openCapabilityTab(page, "Tools & connections");
  await openCapabilityDisclosure(page, "Enroll a provider");
  const providerForm = page.locator("#capability-provider-form");
  await providerForm.getByLabel("Connection name").fill("secondary");
  await providerForm.getByLabel("Provider type").selectOption("openai");
  await providerForm.getByLabel("First model alias").fill("secondary");
  await providerForm.getByLabel("Provider model ID").fill("fixture-secondary");
  await providerForm.getByLabel("Credential").fill(credential);
  const enrollment = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/providers"),
  );
  await providerForm.getByRole("button", { name: "Enroll provider", exact: true }).click();

  expect((await enrollment).status()).toBe(202);
  await expect(page.getByText("Capabilities are current.", { exact: true })).toBeVisible();
  await expect(providerForm.getByLabel("Credential")).toHaveValue("");
  await openCapabilityTab(page, "Models");
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

async function openCapabilityTab(page, name) {
  await page.locator(".capability-tabs").getByRole("link", { name, exact: true }).click();
}

async function openCapabilityDisclosure(page, summary) {
  const disclosure = page.locator("details.capability-disclosure").filter({ hasText: summary });
  if (!(await disclosure.evaluate((element) => element.open))) {
    await disclosure.locator("summary").click();
  }
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
