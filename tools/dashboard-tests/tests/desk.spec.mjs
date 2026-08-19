import { spawn } from "node:child_process";
import http from "node:http";
import { fileURLToPath } from "node:url";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import { existsSync } from "node:fs";

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

function hasVisualBaseline(snapshotName) {
  const parsed = path.parse(snapshotName);
  const baselinePath = path.join(
    testsDir,
    "desk.spec.mjs-snapshots",
    `${parsed.name}-${process.platform}${parsed.ext}`,
  );
  return existsSync(baselinePath) || process.env.WAFFLE_VISUAL_BASELINES === "1";
}

function contrastRatio(foreground, background) {
  const channels = (value) => {
    const match = value.match(/rgba?\(([^)]+)\)/);
    if (!match) {
      throw new Error(`expected an RGB color, got ${value}`);
    }
    return match[1].split(",").slice(0, 3).map((part) => Number(part.trim())).map((channel) => {
      const normalized = channel / 255;
      return normalized <= 0.03928
        ? normalized / 12.92
        : ((normalized + 0.055) / 1.055) ** 2.4;
    });
  };
  const luminance = (value) => {
    const [red, green, blue] = channels(value);
    return red * 0.2126 + green * 0.7152 + blue * 0.0722;
  };
  const light = Math.max(luminance(foreground), luminance(background));
  const dark = Math.min(luminance(foreground), luminance(background));
  return (light + 0.05) / (dark + 0.05);
}

function shadowColor(shadow) {
  const match = shadow.match(/rgba?\([^)]*\)/);
  if (!match) {
    throw new Error(`expected an RGB focus ring, got ${shadow}`);
  }
  return match[0];
}

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({}, testInfo) => {
  testInfo.setTimeout(120_000);
  ({ child: server, url: baseURL } = await startFixture());
});

test.afterAll(async () => {
  await stopFixture(server);
});

// Every rendered test fails on unexpected console errors and page errors
// (#469). Known benign noise is allowlisted; anything else is a regression.
let consoleFailures = [];
let pageerrorFailures = [];
// Tests that intentionally trigger an error response (a validation failure,
// an owner-only export denial, ...) opt in before doing so (#469).
let allowedDiagnostics = [];

function allowDiagnostics(...patterns) {
  allowedDiagnostics.push(...patterns);
}

function allowExpectedResponse(status, path) {
  allowDiagnostics(
    `response ${status} ${baseURL}${path}`,
    `Response Status Error Code ${status} from ${path}`,
  );
}

test.beforeEach(async ({ page }) => {
  allowedDiagnostics = [];



  consoleFailures = [];
  pageerrorFailures = [];
  page.on("console", (message) => {
    if (message.type() !== "error" && message.type() !== "warning") {
      return;
    }
    const text = message.text();
    if (
      text.includes("favicon") ||
      (text.includes("Failed to load resource") && text.includes("404")) ||
      text.includes("net::ERR_ABORTED") ||
      text.includes("SpeechSynthesis") ||
      text.includes("speechSynthesis")
    ) {
      return;
    }
    consoleFailures.push(`${message.type()}: ${text}`);
  });
  page.on("pageerror", (error) => {
    pageerrorFailures.push(String(error));
  });
  page.on("response", (response) => {
    if (response.status() >= 400) {
      consoleFailures.push(`response ${response.status()} ${response.url()}`);
    }
  });
});

test.afterEach(async () => {
  const filtered = consoleFailures.filter(
    (entry) => !allowedDiagnostics.some((pattern) => entry.includes(pattern)),
  );
  const unexpected = [
    ...filtered.map((entry) => `console ${entry}`),
    ...pageerrorFailures.map((entry) => `pageerror ${entry}`),
  ];
  if (unexpected.length > 0) {
    throw new Error(`Unexpected page diagnostics:\n${unexpected.join("\n")}`);
  }
});

// Obstruction detection (#469): at mobile widths the fixed bottom navigation
// must not cover the section's last interactive content, and nothing may be
// clipped vertically by a viewport-sized container.
async function expectSectionObstructionFree(page, section, lastSelector) {
  await page.goto(deskURL(section));
  const metrics = await page.evaluate(
    ({ lastSelector }) => {
      const nav = document.querySelector(".desk-navigation");
      const last = document.querySelector(lastSelector);
      const navRect = nav ? nav.getBoundingClientRect() : null;
      const lastRect = last ? last.getBoundingClientRect() : null;
      return {
        viewport: window.innerHeight,
        navTop: navRect ? navRect.top : null,
        navHeight: navRect ? navRect.height : 0,
        lastBottom: lastRect ? lastRect.bottom : null,
        scrollHeight: document.documentElement.scrollHeight,
        clientHeight: document.documentElement.clientHeight,
      };
    },
    { lastSelector },
  );
  if (metrics.navTop !== null && metrics.lastBottom !== null) {
    // The last interactive element must sit above the fixed nav.
    expect(metrics.lastBottom).toBeLessThanOrEqual(metrics.navTop + 1);
  }
  // No viewport-sized clipping: the document may scroll, but it must be
  // possible to reach the bottom of the content.
  expect(metrics.scrollHeight).toBeGreaterThanOrEqual(metrics.viewport);
}

function rectIntersects(first, second) {
  return first.left < second.right && first.right > second.left && first.top < second.bottom && first.bottom > second.top;
}

async function expectControlsClearOfNavigation(page, selectors) {
  const metrics = [];
  for (const selector of selectors) {
    const targets = page.locator(selector);
    const count = await targets.count();
    expect(count, `${selector} is missing`).toBeGreaterThan(0);
    for (let index = 0; index < count; index += 1) {
      const target = targets.nth(index);
      await expect(target).toBeVisible();
      if (await target.isEnabled().catch(() => false)) {
        await target.focus();
        await expect(target).toBeFocused();
      }
      await target.evaluate((element) => element.scrollIntoView({ block: "center", inline: "nearest", behavior: "instant" }));
      metrics.push(await target.evaluate((element, identity) => {
        const nav = document.querySelector(".desk-navigation")?.getBoundingClientRect();
        const rect = element.getBoundingClientRect();
        return {
          nav: nav && { left: nav.left, right: nav.right, top: nav.top, bottom: nav.bottom },
          rect: { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom },
          viewport: { width: window.innerWidth, height: window.innerHeight },
          navigationHeight: nav?.height ?? 0,
          measuredHeight: parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--desk-navigation-height")) || 0,
          topClearance: parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--desk-top-clearance")) || 0,
          selector: identity.selector,
          index: identity.index,
        };
      }, { selector, index }));
    }
  }
  for (const metric of metrics) {
    expect(metric.nav).not.toBeNull();
    expect(metric.rect).not.toBeNull();
    expect(metric.navigationHeight).toBeGreaterThan(0);
    expect(metric.measuredHeight).toBeCloseTo(metric.navigationHeight, 0);
    expect(metric.rect.top, `${metric.selector}[${metric.index}] top`).toBeGreaterThanOrEqual(metric.topClearance - 1);
    expect(metric.rect.bottom, `${metric.selector}[${metric.index}] bottom`).toBeLessThanOrEqual(metric.viewport.height + 1);
    expect(metric.rect.bottom, `${metric.selector}[${metric.index}] above navigation`).toBeLessThanOrEqual(metric.nav.top + 1);
    expect(rectIntersects(metric.rect, metric.nav), `${metric.selector}[${metric.index}] intersects fixed navigation`).toBe(false);
  }
}

async function expectTodayComposerClearance(page) {
  // Textarea field-sizing and ResizeObserver delivery happen on separate
  // render steps. Wait for the measured CSS contract to catch up with the
  // rendered boxes before asserting the settled geometry.
  await expect.poll(() => page.evaluate(() => {
    const root = getComputedStyle(document.documentElement);
    const composer = document.querySelector("#desk-composer")?.getBoundingClientRect();
    const actions = document.querySelector(".composer-actions")?.getBoundingClientRect();
    const composerVariable = parseFloat(root.getPropertyValue("--desk-composer-height")) || 0;
    const actionVariable = parseFloat(root.getPropertyValue("--desk-action-height")) || 0;
    return Math.max(
      Math.abs(composerVariable - (composer?.height ?? 0)),
      Math.abs(actionVariable - (actions?.height ?? 0)),
    );
  }), "measured composer clearance catches up to rendered geometry").toBeLessThan(0.5);
  const layout = await page.evaluate(() => {
    const root = getComputedStyle(document.documentElement);
    const nav = document.querySelector(".desk-navigation")?.getBoundingClientRect();
    const composer = document.querySelector("#desk-composer")?.getBoundingClientRect();
    const actions = document.querySelector(".composer-actions")?.getBoundingClientRect();
    const transcript = document.querySelector("#desk-transcript");
    const transcriptStyle = transcript ? getComputedStyle(transcript) : null;
    return {
      navTop: nav?.top ?? null,
      navHeight: nav?.height ?? 0,
      composerHeight: composer?.height ?? 0,
      actionHeight: actions?.height ?? 0,
      actionBottom: actions?.bottom ?? null,
      transcriptOverflowY: transcriptStyle?.overflowY ?? "",
      transcriptClientHeight: transcript?.clientHeight ?? 0,
      transcriptScrollHeight: transcript?.scrollHeight ?? 0,
      navigationVariable: parseFloat(root.getPropertyValue("--desk-navigation-height")) || 0,
      composerVariable: parseFloat(root.getPropertyValue("--desk-composer-height")) || 0,
      actionVariable: parseFloat(root.getPropertyValue("--desk-action-height")) || 0,
      viewportHeight: window.innerHeight,
    };
  });
  expect(layout.navTop).not.toBeNull();
  expect(layout.actionBottom).not.toBeNull();
  expect(layout.navigationVariable).toBeCloseTo(layout.navHeight, 0);
  expect(layout.composerVariable).toBeCloseTo(layout.composerHeight, 0);
  expect(layout.actionVariable).toBeCloseTo(layout.actionHeight, 0);
  expect(layout.transcriptOverflowY).toBe("auto");
  expect(layout.actionBottom).toBeLessThanOrEqual(layout.navTop + 1);
  if (layout.composerHeight + layout.navHeight > layout.viewportHeight && layout.transcriptScrollHeight > layout.transcriptClientHeight) {
    // A tall composer keeps a real transcript scroll container; the action
    // row remains a sibling above the fixed navigation instead of becoming
    // the hidden scroll target beneath it.
    expect(layout.transcriptClientHeight).toBeGreaterThan(0);
    const scrollState = await page.locator("#desk-transcript").evaluate((element) => {
      element.scrollTo({ top: element.scrollHeight, behavior: "instant" });
      return { scrollTop: element.scrollTop, scrollable: element.scrollHeight > element.clientHeight };
    });
    expect(scrollState.scrollable).toBe(true);
    expect(scrollState.scrollTop).toBeGreaterThan(0);
    const actionBottom = await page.locator(".composer-actions").evaluate((element) => element.getBoundingClientRect().bottom);
    const navTop = await page.locator(".desk-navigation").evaluate((element) => element.getBoundingClientRect().top);
    expect(actionBottom).toBeLessThanOrEqual(navTop + 1);
  }
}

async function expectVisibleComposerActionsClearOfNavigation(page, label) {
  const geometry = await page.evaluate(() => {
    const navigation = document.querySelector(".desk-navigation")?.getBoundingClientRect();
    const actions = Array.from(document.querySelectorAll(".composer-actions button"))
      .filter((element) => {
        const style = getComputedStyle(element);
        return !element.hidden && style.display !== "none" && style.visibility !== "hidden";
      })
      .map((element) => {
        const bounds = element.getBoundingClientRect();
        return {
          id: element.id,
          left: bounds.left,
          right: bounds.right,
          top: bounds.top,
          bottom: bounds.bottom,
        };
      });
    return {
      navigation: navigation && {
        left: navigation.left,
        right: navigation.right,
        top: navigation.top,
        bottom: navigation.bottom,
      },
      actions,
    };
  });
  expect(geometry.navigation).not.toBeNull();
  expect(geometry.actions.length, `${label} has visible composer actions`).toBeGreaterThan(0);
  for (const action of geometry.actions) {
    expect(action.bottom, `${label} #${action.id} above navigation`).toBeLessThanOrEqual(geometry.navigation.top + 1);
    expect(rectIntersects(action, geometry.navigation), `${label} #${action.id} intersects navigation`).toBe(false);
  }
  for (let first = 0; first < geometry.actions.length; first += 1) {
    for (let second = first + 1; second < geometry.actions.length; second += 1) {
      expect(
        rectIntersects(geometry.actions[first], geometry.actions[second]),
        `${label} #${geometry.actions[first].id} overlaps #${geometry.actions[second].id}`,
      ).toBe(false);
    }
  }
  return geometry;
}

async function expectMobileUtilityHeaderClearance(page) {
  await page.evaluate(() => {
    window.scrollTo(0, 0);
    document.querySelector("main")?.scrollTo(0, 0);
  });
  const metrics = await page.evaluate(() => {
    const brand = document.querySelector(".brand")?.getBoundingClientRect();
    const palette = document.querySelector("#palette-open")?.getBoundingClientRect();
    const header = document.querySelector(".today-header")?.getBoundingClientRect();
    return {
      brand: brand && { left: brand.left, right: brand.right, top: brand.top, bottom: brand.bottom },
      palette: palette && { left: palette.left, right: palette.right, top: palette.top, bottom: palette.bottom },
      header: header && { top: header.top, bottom: header.bottom },
    };
  });
  expect(metrics.brand).not.toBeNull();
  expect(metrics.palette).not.toBeNull();
  expect(metrics.header).not.toBeNull();
  expect(metrics.brand.bottom).toBeLessThanOrEqual(metrics.header.top + 1);
  expect(metrics.palette.bottom).toBeLessThanOrEqual(metrics.header.top + 1);
  expect(metrics.brand.right).toBeLessThanOrEqual(metrics.palette.left);
  expect(Math.max(metrics.brand.bottom, metrics.palette.bottom) - Math.min(metrics.brand.top, metrics.palette.top)).toBeLessThanOrEqual(64);
}

async function expectLastFocusableClear(page, sectionSelector) {
  const last = page.locator(`${sectionSelector} a[href]:visible, ${sectionSelector} button:not([disabled]):visible, ${sectionSelector} input:not([disabled]):visible, ${sectionSelector} select:not([disabled]):visible, ${sectionSelector} textarea:not([disabled]):visible`).last();
  await expect(last).toBeVisible();
  await last.focus();
  await expect(last).toBeFocused();
  await last.scrollIntoViewIfNeeded();
  const metrics = await last.evaluate((element) => {
    const nav = document.querySelector(".desk-navigation")?.getBoundingClientRect();
    const rect = element.getBoundingClientRect();
    return {
      nav: nav && { left: nav.left, right: nav.right, top: nav.top, bottom: nav.bottom },
      rect: { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom },
      viewport: { width: window.innerWidth, height: window.innerHeight },
    };
  });
  expect(metrics.nav).not.toBeNull();
  expect(metrics.rect.top, `${sectionSelector} last focusable top`).toBeGreaterThanOrEqual(0);
  expect(metrics.rect.bottom, `${sectionSelector} last focusable bottom`).toBeLessThanOrEqual(metrics.viewport.height + 1);
  expect(metrics.rect.bottom, `${sectionSelector} last focusable above navigation`).toBeLessThanOrEqual(metrics.nav.top + 1);
  expect(rectIntersects(metrics.rect, metrics.nav), `${sectionSelector} last focusable intersects navigation`).toBe(false);
}

test("Capabilities actions are compact, touch-sized, and honest at desktop and mobile widths", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the explicit Capabilities geometry contract once.");
  for (const viewport of [{ width: 1470, height: 1000 }, { width: 375, height: 812 }]) {
    await page.setViewportSize(viewport);
    await page.goto(deskURL("capabilities"));
    await openCapabilityTab(page, "Models");
    const cards = page.locator("#capability-models .capability-card");
    await expect(cards.first()).toBeVisible();
    const geometry = await cards.first().evaluate((card) => {
      const row = card.querySelector(".waffle-fragment-actions");
      const cardRect = card.getBoundingClientRect();
      const buttons = [...card.querySelectorAll(".waffle-fragment-actions button")].map((button) => {
        const rect = button.getBoundingClientRect();
        return { width: rect.width, height: rect.height, top: rect.top, left: rect.left, right: rect.right };
      });
      return {
        rowDisplay: row && getComputedStyle(row).display,
        cardWidth: cardRect.width,
        buttons,
      };
    });
    expect(geometry.rowDisplay).toBe("flex");
    expect(geometry.buttons).toHaveLength(2);
    for (const button of geometry.buttons) {
      expect(button.height).toBeGreaterThanOrEqual(44);
      expect(button.width).toBeLessThan(geometry.cardWidth * 0.9);
    }
    if (viewport.width === 1470) {
      expect(Math.abs(geometry.buttons[0].top - geometry.buttons[1].top)).toBeLessThanOrEqual(1);
    }

    const defaultCard = cards.filter({ hasText: "Waffle-wide default" }).first();
    await expect(defaultCard.getByRole("button", { name: "Default", exact: true })).toBeDisabled();
    await expect(defaultCard.getByRole("button", { name: "Default", exact: true })).toHaveAttribute("aria-pressed", "true");
    const utilityCard = cards.filter({ hasText: "Utility model" }).first();
    await expect(utilityCard.getByRole("button", { name: "Utility model", exact: true })).toBeDisabled();
    await expect(utilityCard.getByRole("button", { name: "Utility model", exact: true })).toHaveAttribute("aria-pressed", "true");

    for (const [tab, listID] of [["Skills", "#capability-skills"], ["Tools & connections", "#capability-connections"]]) {
      await page.goto(deskURL("capabilities"));
      await openCapabilityTab(page, tab);
      const actionRow = page.locator(`${listID} .capability-card .waffle-fragment-actions`).first();
      if (await actionRow.count() === 0) {
        continue;
      }
      await expect(actionRow).toBeVisible();
      const rowMetrics = await actionRow.evaluate((row) => {
        const button = row.querySelector("button");
        const rect = button?.getBoundingClientRect();
        return { display: getComputedStyle(row).display, height: rect?.height ?? 0, width: rect?.width ?? 0 };
      });
      expect(rowMetrics.display).toBe("flex");
      expect(rowMetrics.height).toBeGreaterThanOrEqual(44);
      expect(rowMetrics.width).toBeLessThan(geometry.cardWidth * 0.9);
    }
  }
});

test("shared navigation reserves its measured bar and clears every section at required widths", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the explicit fixed-navigation contract once.");
  // The fixture closes Today ownership when navigating sections; its next
  // attach reports the expected recovery 404 before opening a fresh turn.
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  const widths = [
    { width: 320, height: 812 },
    { width: 375, height: 812 },
    { width: 768, height: 1000 },
  ];
  for (const viewport of widths) {
    await page.setViewportSize(viewport);
    await page.goto(deskURL("today"));
    await expect(page.locator("#desk-phase")).toHaveText("Ready");
    await expectMobileUtilityHeaderClearance(page);
    await expectControlsClearOfNavigation(page, ["#desk-message", "#desk-send", "#desk-cancel"]);
    await expectTodayComposerClearance(page);
    await expect(page.locator("#palette-open")).toBeVisible();
    await page.locator("#palette-open").click();
    await expect(page.locator("#command-palette")).toBeVisible();
    expect(await page.locator("#command-palette").evaluate((element) => Number.parseInt(getComputedStyle(element).zIndex, 10))).toBeGreaterThan(
      await page.locator(".desk-navigation").evaluate((element) => Number.parseInt(getComputedStyle(element).zIndex, 10)),
    );
    await page.keyboard.press("Escape");

    for (const [section, selector] of [
      ["today", ".today"],
      ["tasks", ".tasks"],
      ["workspaces", ".workspaces"],
      ["memory", ".memory"],
      ["capabilities", "#desk-capabilities"],
    ]) {
      await page.goto(deskURL(section));
      if (section === "today") {
        await expect(page.locator("#desk-phase")).toHaveText("Ready");
      } else if (section === "capabilities") {
        await openCapabilityTab(page, "Models");
        await expect(page.locator("#capability-models .capability-card").first()).toBeVisible();
        await expectControlsClearOfNavigation(page, ["#capability-models .capability-card .waffle-fragment-actions button"]);
      } else if (section === "tasks") {
        await expect(page.locator("#tasks-list")).toHaveAttribute("data-waffle-fragment", "true");
        const scheduleDialog = page.locator("#task-schedule-dialog");
        await page.locator("#task-schedule-open").click();
        await expect(scheduleDialog).toBeVisible();
        await expect.poll(() => scheduleDialog.evaluate((element) => element.matches(":modal"))).toBe(true);
        await scheduleDialog.getByRole("button", { name: "Cancel", exact: true }).click();
        await expect(scheduleDialog).toBeHidden();
      } else if (section === "workspaces") {
        await expect(page.locator("#workspaces-list")).toHaveAttribute("data-waffle-fragment", "true");
      }
      await expectLastFocusableClear(page, selector);
    }
  }
});

test("Today keeps a long transcript scrollable while the real composer remains above navigation", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the explicit dynamic composer contract once.");
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  const widths = [
    { width: 320, height: 812 },
    { width: 375, height: 812 },
    { width: 768, height: 1000 },
  ];
  for (const viewport of widths) {
    await page.setViewportSize(viewport);
    await page.goto(deskURL("today"));
    await expect(page.locator("#desk-phase")).toHaveText("Ready");
    await page.locator("#desk-message").fill("/");
    await expect(page.locator("#desk-slash-menu")).toBeVisible();
    const initialTextareaHeight = await page.locator("#desk-message").evaluate((element) => element.getBoundingClientRect().height);
    await page.locator("#desk-message").fill(`${"A deliberately long composer draft ".repeat(160)}/`);
    const grownTextareaHeight = await page.locator("#desk-message").evaluate((element) => element.getBoundingClientRect().height);
    expect(grownTextareaHeight).toBeGreaterThan(initialTextareaHeight);
    await expect(page.locator("#desk-slash-menu")).toBeVisible();
    await page.locator("#desk-transcript").evaluate((transcript) => {
      transcript.replaceChildren(...Array.from({ length: 30 }, (_, index) => {
        const row = document.createElement("p");
        row.className = "waffle-message assistant-message";
        row.textContent = `Injected long transcript row ${index}: ${"A settled response that must remain inside the shrinking transcript. ".repeat(16)}`;
        return row;
      }));
    });
    await expect.poll(() => page.locator("#desk-transcript").evaluate((element) => ({
      clientHeight: element.clientHeight,
      scrollHeight: element.scrollHeight,
    }))).toEqual(expect.objectContaining({ clientHeight: expect.any(Number) }));
    const transcriptState = await page.locator("#desk-transcript").evaluate((element) => {
      element.scrollTo({ top: element.scrollHeight, behavior: "instant" });
      return {
        clientHeight: element.clientHeight,
        scrollHeight: element.scrollHeight,
        scrollTop: element.scrollTop,
      };
    });
    expect(transcriptState.scrollHeight).toBeGreaterThan(transcriptState.clientHeight);
    expect(transcriptState.scrollTop).toBeGreaterThan(0);
    const geometry = await page.evaluate(() => {
      const rect = (selector) => {
        const element = document.querySelector(selector);
        if (!element) {
          return null;
        }
        const bounds = element.getBoundingClientRect();
        return { left: bounds.left, right: bounds.right, top: bounds.top, bottom: bounds.bottom };
      };
      const root = getComputedStyle(document.documentElement);
      const slashMenu = document.querySelector("#desk-slash-menu");
      const slashBounds = slashMenu?.getBoundingClientRect();
      const conversationBounds = document.querySelector(".conversation")?.getBoundingClientRect();
      const columnsBounds = document.querySelector(".today-columns")?.getBoundingClientRect();
      const slashVisibleTop = Math.max(slashBounds?.top ?? 0, conversationBounds?.top ?? 0, columnsBounds?.top ?? 0, 0);
      const slashVisibleBottom = Math.min(
        slashBounds?.bottom ?? 0,
        conversationBounds?.bottom ?? 0,
        columnsBounds?.bottom ?? 0,
        window.innerHeight,
      );
      return {
        message: rect("#desk-message"),
        composer: rect("#desk-composer"),
        actions: rect(".composer-actions"),
        send: rect("#desk-send"),
        cancel: rect("#desk-cancel"),
        transcript: rect("#desk-transcript"),
        slashMenu: rect("#desk-slash-menu"),
        conversation: rect(".conversation"),
        todayColumns: rect(".today-columns"),
        navigation: rect(".desk-navigation"),
        viewport: { width: window.innerWidth, height: window.innerHeight },
        topClearance: parseFloat(root.getPropertyValue("--desk-top-clearance")) || 0,
        slashMenuStickyTop: slashMenu ? parseFloat(getComputedStyle(slashMenu).top) || 0 : 0,
        slashMenuVisibleHeight: Math.max(0, slashVisibleBottom - slashVisibleTop),
        slashMenuPainted: slashBounds && slashVisibleBottom > slashVisibleTop
          ? slashMenu.contains(document.elementFromPoint(
              slashBounds.left + slashBounds.width / 2,
              slashVisibleTop + (slashVisibleBottom - slashVisibleTop) / 2,
            ))
          : false,
      };
    });
    expect(geometry.slashMenu).not.toBeNull();
    expect(geometry.conversation).not.toBeNull();
    expect(geometry.navigation).not.toBeNull();
    expect(geometry.slashMenuStickyTop, `${viewport.width}px slash menu keeps a positive sticky inset`).toBeGreaterThan(0);
    expect(geometry.slashMenuVisibleHeight, `${viewport.width}px slash menu retains a painted touch row`).toBeGreaterThanOrEqual(44);
    expect(geometry.slashMenuPainted, `${viewport.width}px slash menu is painted inside the compact scrollport`).toBe(true);
    expect(geometry.slashMenu.top, `${viewport.width}px slash menu stays inside the clipped conversation top`).toBeGreaterThanOrEqual(
      geometry.conversation.top - 1,
    );
    expect(geometry.slashMenu.bottom, `${viewport.width}px slash menu stays inside the clipped conversation bottom`).toBeLessThanOrEqual(
      geometry.conversation.bottom + 1,
    );
    const simultaneousTargets = ["message", "actions", "send", "cancel", "slashMenu"];
    for (const target of simultaneousTargets) {
      expect(geometry[target], `${target} is visible in the settled geometry`).not.toBeNull();
      expect(geometry[target].top, `${target} top`).toBeGreaterThanOrEqual(geometry.topClearance - 1);
      expect(geometry[target].top, `${target} viewport top`).toBeGreaterThanOrEqual(-1);
      expect(geometry[target].bottom, `${target} viewport bottom`).toBeLessThanOrEqual(geometry.viewport.height + 1);
      expect(geometry[target].bottom, `${target} above navigation`).toBeLessThanOrEqual(geometry.navigation.top + 1);
      expect(rectIntersects(geometry[target], geometry.navigation), `${target} intersects navigation`).toBe(false);
    }
    expect(geometry.composer).not.toBeNull();
    expect(geometry.transcript).not.toBeNull();
    await expectTodayComposerClearance(page);
    await expectMobileUtilityHeaderClearance(page);
    await page.locator("#palette-open").click();
    await expect(page.locator("#command-palette")).toBeVisible();
    await page.keyboard.press("Escape");
    const trailing = await page.evaluate(() => {
      const main = document.querySelector("main");
      const shell = document.querySelector(".desk-shell");
      const nav = document.querySelector(".desk-navigation")?.getBoundingClientRect();
      return {
        documentHeight: document.documentElement.scrollHeight,
        bodyHeight: document.body.scrollHeight,
        mainHeight: main?.scrollHeight ?? 0,
        mainRect: main ? (() => { const rect = main.getBoundingClientRect(); return { top: rect.top, bottom: rect.bottom, height: rect.height }; })() : null,
        shellHeight: shell?.scrollHeight ?? 0,
        shellRect: shell ? (() => { const rect = shell.getBoundingClientRect(); return { top: rect.top, bottom: rect.bottom, height: rect.height }; })() : null,
        todayRect: (() => { const rect = document.querySelector(".today")?.getBoundingClientRect(); return rect ? { top: rect.top, bottom: rect.bottom, height: rect.height } : null; })(),
        columnsRect: (() => { const rect = document.querySelector(".today-columns")?.getBoundingClientRect(); return rect ? { top: rect.top, bottom: rect.bottom, height: rect.height } : null; })(),
        navHeight: nav?.height ?? 0,
        viewportHeight: window.innerHeight,
      };
    });
    expect(trailing.bodyHeight - trailing.viewportHeight, JSON.stringify(trailing)).toBeLessThanOrEqual(24);
    expect(trailing.shellHeight - trailing.mainHeight, JSON.stringify(trailing)).toBeLessThanOrEqual(trailing.navHeight + 24);
  }
});

test("Today Hearth centers the reading column and exercises the 15rem composer at every required width", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the explicit Hearth geometry contract once.");
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  const viewports = [
    { width: 1414, height: 920 },
    { width: 768, height: 1000 },
    { width: 375, height: 812 },
    { width: 320, height: 812 },
  ];
  for (const viewport of viewports) {
    await page.setViewportSize(viewport);
    await page.goto(deskURL("today"));
    await expect(page.locator("#desk-phase")).toHaveText("Ready");

    const column = page.locator(".conversation-column");
    await expect(column).toBeVisible();
    await page.locator("#desk-message").fill("");
    const initialHeight = await page.locator("#desk-message").evaluate(
      (element) => element.getBoundingClientRect().height,
    );
    expect(initialHeight, `${viewport.width}px empty composer height`).toBeLessThanOrEqual(48);
    if (viewport.width <= 768) {
      const initialClearance = await page.evaluate(() => ({
        composerBottom: document.querySelector("#desk-composer").getBoundingClientRect().bottom,
        navigationTop: document.querySelector(".desk-navigation").getBoundingClientRect().top,
      }));
      expect(initialClearance.composerBottom, `${viewport.width}px empty composer clearance`)
        .toBeLessThanOrEqual(initialClearance.navigationTop + 1);
      await expectVisibleComposerActionsClearOfNavigation(page, `${viewport.width}px empty composer`);
    }

    await page.locator("#desk-message").fill(
      Array.from({ length: 20 }, (_, index) => `line ${index + 1}`).join("\n"),
    );
    if (viewport.width <= 768) {
      await expect.poll(async () => page.evaluate(() => {
        const composer = document.querySelector("#desk-composer").getBoundingClientRect();
        const navigation = document.querySelector(".desk-navigation").getBoundingClientRect();
        return composer.bottom - navigation.top;
      })).toBeLessThanOrEqual(1);
    }
    const geometry = await page.evaluate(() => {
      const rect = (selector) => {
        const value = document.querySelector(selector)?.getBoundingClientRect();
        return value && {
          left: value.left,
          right: value.right,
          top: value.top,
          bottom: value.bottom,
          width: value.width,
          height: value.height,
        };
      };
      const conversation = rect(".conversation");
      const column = rect(".conversation-column");
      const transcript = document.querySelector("#desk-transcript");
      const textarea = document.querySelector("#desk-message");
      const navigation = rect(".desk-navigation");
      const controls = [
        "#desk-model", "#desk-skill", "#desk-task-mode", "#desk-reasoning",
        "#desk-cancel", "#desk-schedule-draft", "#desk-dictate",
        "#desk-attach-button", "#desk-send",
      ].map(rect);
      const composerActions = [
        "#desk-cancel", "#desk-schedule-draft", "#desk-dictate",
        "#desk-attach-button", "#desk-send",
      ].map((selector) => ({ selector, ...rect(selector) }));
      return {
        conversation,
        column,
        transcript: rect("#desk-transcript"),
        composer: rect("#desk-composer"),
        textarea: rect("#desk-message"),
        textareaOverflowY: getComputedStyle(textarea).overflowY,
        textareaScrollHeight: textarea.scrollHeight,
        textareaClientHeight: textarea.clientHeight,
        transcriptOverflowY: getComputedStyle(transcript).overflowY,
        navigation,
        controls,
        composerActions,
      };
    });
    expect(geometry.column.width, `${viewport.width}px column width`).toBeLessThanOrEqual(832.5);
    expect(
      Math.abs(
        geometry.column.left - geometry.conversation.left -
        (geometry.conversation.right - geometry.column.right)
      ),
      `${viewport.width}px centered column`,
    ).toBeLessThanOrEqual(1.5);
    expect(geometry.transcript.width).toBeLessThanOrEqual(832.5);
    expect(geometry.composer.width).toBeLessThanOrEqual(832.5);
    expect(geometry.transcriptOverflowY).toBe("auto");
    expect(geometry.textarea.height, `${viewport.width}px textarea cap`).toBeCloseTo(240, 0);
    expect(geometry.textareaOverflowY).toBe("auto");
    expect(geometry.textareaScrollHeight).toBeGreaterThan(geometry.textareaClientHeight);
    for (const control of geometry.controls) {
      expect(control).not.toBeNull();
      expect(control.height, `${viewport.width}px touch target`).toBeGreaterThanOrEqual(44);
      expect(control.left).toBeGreaterThanOrEqual(geometry.composer.left - 1);
      expect(control.right).toBeLessThanOrEqual(geometry.composer.right + 1);
    }
    for (let first = 0; first < geometry.controls.length; first += 1) {
      for (let second = first + 1; second < geometry.controls.length; second += 1) {
        expect(
          rectIntersects(geometry.controls[first], geometry.controls[second]),
          `${viewport.width}px controls ${first} and ${second} overlap`,
        ).toBe(false);
      }
    }
    if (viewport.width <= 768) {
      expect(geometry.composer.bottom).toBeLessThanOrEqual(geometry.navigation.top + 1);
      expect(
        rectIntersects(geometry.composer, geometry.navigation),
        `${viewport.width}px composer intersects navigation: ${JSON.stringify({ composer: geometry.composer, navigation: geometry.navigation })}`,
      ).toBe(false);
      for (const action of geometry.composerActions) {
        expect(action.bottom, `${viewport.width}px ${action.selector} above navigation`)
          .toBeLessThanOrEqual(geometry.navigation.top + 1);
      }
      await expectVisibleComposerActionsClearOfNavigation(page, `${viewport.width}px 15rem composer`);
    }
  }
});

test("Hearth and Evening show a representative Today transcript at desktop and compact widths", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Capture the representative Task 3 frames once.");
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  await page.route("**/api/v1/desk/setup", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ complete: true, steps: [] }),
    });
  });
  for (const [theme, viewport, size, snapshot] of [
    ["light", { width: 1414, height: 920 }, "desktop", true],
    ["light", { width: 375, height: 812 }, "compact", true],
    ["dark", { width: 1414, height: 920 }, "desktop", true],
    ["dark", { width: 375, height: 812 }, "compact", true],
    ["light", { width: 320, height: 812 }, "narrow", false],
  ]) {
    await page.setViewportSize(viewport);
    await page.goto(deskURL("today"));
    const expectRootAtTop = async (step) => {
      if (viewport.width > 375) return;
      const root = await page.evaluate(() => ({
        documentScrollTop: document.scrollingElement.scrollTop,
        windowScrollY: window.scrollY,
        active: document.activeElement?.id || document.activeElement?.className || document.activeElement?.tagName,
      }));
      expect(root.documentScrollTop, `${viewport.width}px root moved after ${step}: ${JSON.stringify(root)}`).toBe(0);
      expect(root.windowScrollY, `${viewport.width}px window moved after ${step}: ${JSON.stringify(root)}`).toBe(0);
    };
    await page.getByLabel("Theme").selectOption(theme);
    await expect(page.locator("#desk-phase")).toHaveText("Ready");
    await expectRootAtTop("open");
    await page.getByLabel("Message Waffle").fill("hearth visual");
    await expectRootAtTop("fill");
    await page.getByRole("button", { name: "Send message", exact: true }).click();
    await expectRootAtTop("send");
    await page.locator(".message-branch").last().evaluate((button) => {
      const transcript = button.closest("#desk-transcript");
      const bounds = button.getBoundingClientRect();
      const transcriptBounds = transcript.getBoundingClientRect();
      transcript.scrollTo({
        top: transcript.scrollTop + Math.max(0, bounds.bottom - transcriptBounds.bottom + 8),
        behavior: "instant",
      });
    });
    await page.locator(".message-branch").last().evaluate((button) => {
      button.focus({ preventScroll: true });
    });
    await page.keyboard.press("Enter");
    await expectRootAtTop("branch");
    const reasoning = page.locator(".message-reasoning");
    await expect(reasoning).toBeVisible();
    await expect(reasoning).not.toHaveAttribute("open", "");
    await expect(reasoning.locator("summary")).toHaveText("Reasoning");
    await expect(page.locator(".code-language")).toHaveText("go");
    await expect(page.locator(".code-copy")).toBeVisible();
    await expect(page.locator("#desk-composer")).toBeVisible();
    await expectNoCanaries(page);
    if (viewport.width <= 375) {
      const clearance = await page.evaluate(() => {
        const navigationTop = document.querySelector(".desk-navigation").getBoundingClientRect().top;
        return {
          navigationTop,
          mainScrollTop: document.querySelector("main").scrollTop,
          documentScrollTop: document.scrollingElement.scrollTop,
          windowScrollY: window.scrollY,
          actions: [
            "#desk-cancel", "#desk-schedule-draft", "#desk-dictate",
            "#desk-attach-button", "#desk-send",
          ].map((selector) => {
            const bounds = document.querySelector(selector).getBoundingClientRect();
            return { selector, top: bounds.top, bottom: bounds.bottom, left: bounds.left, right: bounds.right };
          }),
        };
      });
      expect(clearance.mainScrollTop, `${viewport.width}px restored main scroll owner`).toBe(0);
      expect(clearance.documentScrollTop, `${viewport.width}px restored document scroll owner`).toBe(0);
      expect(clearance.windowScrollY, `${viewport.width}px restored window scroll owner`).toBe(0);
      for (const action of clearance.actions) {
        expect(action.bottom, `${viewport.width}px restored ${action.selector} above navigation`)
          .toBeLessThanOrEqual(clearance.navigationTop + 1);
      }
      await expectVisibleComposerActionsClearOfNavigation(page, `${viewport.width}px restored composer`);
      for (let first = 0; first < clearance.actions.length; first += 1) {
        for (let second = first + 1; second < clearance.actions.length; second += 1) {
          expect(
            rectIntersects(clearance.actions[first], clearance.actions[second]),
            `${viewport.width}px restored actions ${first} and ${second} overlap`,
          ).toBe(false);
        }
      }
    }
    const snapshotName = `desk-today-${theme}-${size}.png`;
    if (snapshot && hasVisualBaseline(snapshotName)) {
      await expect(page).toHaveScreenshot(snapshotName, {
        animations: "disabled",
        caret: "hide",
        maxDiffPixelRatio: 0.005,
      });
    }
  }
});

test("approved Waffle identity stays visible in Hearth and Evening at desktop and mobile sizes", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the shared identity render contract once.");
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  for (const theme of ["light", "dark"]) {
    for (const viewport of [{ width: 1414, height: 786 }, { width: 375, height: 812 }]) {
      await page.setViewportSize(viewport);
      await page.goto(deskURL("today"));
      if (theme === "dark") {
        await page.getByLabel("Theme").selectOption("dark");
      }
      const mark = page.locator(".brand-waffle");
      await expect(mark).toBeVisible();
      await expect(mark).toHaveAttribute("alt", "");
      await expect(mark).toHaveAttribute("aria-hidden", "true");
      const rendered = await mark.evaluate((element) => {
        const rect = element.getBoundingClientRect();
        return { width: rect.width, height: rect.height, naturalWidth: element.naturalWidth, src: element.getAttribute("src") };
      });
      expect(rendered.width).toBe(28);
      expect(rendered.height).toBe(28);
      expect(rendered.naturalWidth).toBe(128);
      expect(rendered.src).toContain("/desk/assets/waffle-mark-sitting.png?v=");
      await expect(page.getByRole("link", { name: "Waffle Desk home", exact: true })).toHaveCount(1);
    }
  }
});

test("structured empty state stays bounded and quiet in the shared Hearth and Evening shell", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the structured empty-state render contract once.");
  const viewports = [
    { width: 1414, height: 786 },
    { width: 375, height: 812 },
    { width: 320, height: 812 },
  ];
  for (const theme of ["light", "dark"]) {
    for (const viewport of viewports) {
      await page.setViewportSize(viewport);
      await page.goto(`${baseURL}/test/empty-state?theme=${theme}&populated=0&shell=1`);
      await expect(page.locator(".desk-shell")).toBeVisible();
      await expect(page.locator(".desk-navigation")).toBeVisible();
      const empty = page.locator(".waffle-empty-state");
      await expect(empty).toBeVisible();
      const rendered = await empty.evaluate((element) => {
        const image = element.querySelector("img");
        const rect = element.getBoundingClientRect();
        const imageRect = image.getBoundingClientRect();
        const rootStyle = getComputedStyle(document.documentElement);
        return {
          role: element.getAttribute("role"),
          live: element.getAttribute("aria-live"),
          labelledBy: element.getAttribute("aria-labelledby"),
          width: rect.width,
          imageWidth: imageRect.width,
          imageHeight: imageRect.height,
          imageLoading: image.getAttribute("loading"),
          imageDecoding: image.getAttribute("decoding"),
          imageAlt: image.getAttribute("alt"),
          imageHidden: image.getAttribute("aria-hidden"),
          canvas: rootStyle.getPropertyValue("--surface-canvas").trim(),
          scrollWidth: document.documentElement.scrollWidth,
          viewportWidth: window.innerWidth,
        };
      });
      expect(rendered.role).toBe("region");
      expect(rendered.role).not.toBe("status");
      expect(rendered.live).toBeNull();
      expect(rendered.labelledBy).toBe("fixture-empty-state-title");
      expect(rendered.imageLoading).toBe("lazy");
      expect(rendered.imageDecoding).toBe("async");
      expect(rendered.imageAlt).toBe("");
      expect(rendered.imageHidden).toBe("true");
      expect(rendered.imageWidth).toBeGreaterThanOrEqual(120);
      expect(rendered.imageWidth).toBeLessThanOrEqual(160);
      expect(rendered.scrollWidth).toBeLessThanOrEqual(rendered.viewportWidth);
      await expectNoHorizontalOverflow(page);
      await expect(empty.locator(".action-primary")).toBeVisible();
      await expect(empty.locator(".action-quiet")).toBeVisible();
      const actionForm = empty.locator("form").first();
      const actionGeometry = await empty.evaluate((element) => {
        const rect = (selector) => {
          const node = element.querySelector(selector);
          const bounds = node.getBoundingClientRect();
          return { left: bounds.left, right: bounds.right, top: bounds.top, bottom: bounds.bottom };
        };
        const region = element.getBoundingClientRect();
        const copy = element.querySelector(".waffle-empty-state-copy").getBoundingClientRect();
        const style = getComputedStyle(element);
        const paddingLeft = Number.parseFloat(style.paddingLeft) || 0;
        const paddingRight = Number.parseFloat(style.paddingRight) || 0;
        const forms = [...element.querySelectorAll(".waffle-empty-state-actions form")].map((form) => {
          const bounds = form.getBoundingClientRect();
          const button = form.querySelector("button").getBoundingClientRect();
          return {
            className: form.className,
            left: bounds.left,
            right: bounds.right,
            width: bounds.width,
            buttonWidth: button.width,
          };
        });
        return {
          region: { left: region.left, right: region.right, top: region.top, bottom: region.bottom },
          content: { left: region.left + paddingLeft, right: region.right - paddingRight },
          copy: { left: copy.left, right: copy.right, top: copy.top, bottom: copy.bottom },
          forms,
          inputs: [...element.querySelectorAll("form input:not([type=hidden])")].map((input) => {
            const bounds = input.getBoundingClientRect();
            return { left: bounds.left, right: bounds.right, top: bounds.top, bottom: bounds.bottom };
          }),
          viewportWidth: window.innerWidth,
          scrollWidth: document.documentElement.scrollWidth,
        };
      });
      expect(actionGeometry.forms).toHaveLength(2);
      const inputForm = actionGeometry.forms[0];
      const fieldsOnlyForm = actionGeometry.forms[1];
      expect(inputForm.className).toContain("waffle-action-form-with-inputs");
      expect(fieldsOnlyForm.className).not.toContain("waffle-action-form-with-inputs");
      expect(inputForm.left).toBeGreaterThanOrEqual(actionGeometry.copy.left - 1);
      expect(inputForm.right).toBeLessThanOrEqual(actionGeometry.copy.right + 1);
      expect(actionGeometry.copy.left).toBeGreaterThanOrEqual(actionGeometry.content.left - 1);
      expect(actionGeometry.copy.right).toBeLessThanOrEqual(actionGeometry.content.right + 1);
      expect(inputForm.left).toBeGreaterThanOrEqual(actionGeometry.content.left - 1);
      expect(inputForm.right).toBeLessThanOrEqual(actionGeometry.content.right + 1);
      expect(fieldsOnlyForm.left).toBeGreaterThanOrEqual(actionGeometry.copy.left - 1);
      expect(fieldsOnlyForm.right).toBeLessThanOrEqual(actionGeometry.copy.right + 1);
      expect(fieldsOnlyForm.width).toBeLessThan(actionGeometry.copy.right - actionGeometry.copy.left);
      for (const input of actionGeometry.inputs) {
        expect(input.left).toBeGreaterThanOrEqual(actionGeometry.copy.left - 1);
        expect(input.right).toBeLessThanOrEqual(actionGeometry.copy.right + 1);
      }
      expect(actionGeometry.scrollWidth).toBeLessThanOrEqual(actionGeometry.viewportWidth);
      await actionForm.getByLabel("Note", { exact: true }).fill("review payload");
      const filledInputGeometry = await empty.evaluate((element) => {
        const style = getComputedStyle(element);
        const region = element.getBoundingClientRect();
        const contentLeft = region.left + (Number.parseFloat(style.paddingLeft) || 0);
        const contentRight = region.right - (Number.parseFloat(style.paddingRight) || 0);
        const form = element.querySelector("form").getBoundingClientRect();
        const input = element.querySelector("form input:not([type=hidden])").getBoundingClientRect();
        return {
          form: { left: form.left, right: form.right },
          input: { left: input.left, right: input.right },
          content: { left: contentLeft, right: contentRight },
          scrollWidth: document.documentElement.scrollWidth,
          viewportWidth: window.innerWidth,
        };
      });
      expect(filledInputGeometry.form.left).toBeGreaterThanOrEqual(filledInputGeometry.content.left - 1);
      expect(filledInputGeometry.form.right).toBeLessThanOrEqual(filledInputGeometry.content.right + 1);
      expect(filledInputGeometry.input.left).toBeGreaterThanOrEqual(filledInputGeometry.content.left - 1);
      expect(filledInputGeometry.input.right).toBeLessThanOrEqual(filledInputGeometry.content.right + 1);
      expect(filledInputGeometry.scrollWidth).toBeLessThanOrEqual(filledInputGeometry.viewportWidth);
      const actionRequest = page.waitForRequest((request) => request.url().endsWith("/api/v1/desk/test/empty-action"));
      await actionForm.getByRole("button", { name: "Start with Tasks", exact: true }).click();
      const submitted = await actionRequest;
      expect(submitted.method()).toBe("POST");
      expect(submitted.headers()["content-type"]).toContain("application/json");
      expect(JSON.parse(submitted.postData())).toMatchObject({ fixture: "empty-state", note: "review payload" });
      const secondaryRequest = page.waitForRequest((request) => request.url().endsWith("/api/v1/desk/test/empty-action") && request !== submitted);
      await empty.locator("form").nth(1).getByRole("button", { name: "Review later", exact: true }).click();
      const secondarySubmitted = await secondaryRequest;
      expect(secondarySubmitted.method()).toBe("POST");
      expect(secondarySubmitted.headers()["content-type"]).toContain("application/json");
      expect(JSON.parse(secondarySubmitted.postData())).toMatchObject({ fixture: "empty-state-secondary" });
      const emptySnapshot = `desk-empty-state-${theme}-${viewport.width}.png`;
      if (viewport.width !== 320 && hasVisualBaseline(emptySnapshot)) {
        await expect(page).toHaveScreenshot(emptySnapshot, { animations: "disabled", caret: "hide", maxDiffPixelRatio: 0.005 });
      }

      await page.goto(`${baseURL}/test/empty-state?theme=${theme}&populated=1&shell=1`);
      await expect(page.locator(".desk-shell")).toBeVisible();
      await expect(page.locator(".fixture-populated-state")).toBeVisible();
      await expect(page.locator(".waffle-empty-state")).toHaveCount(0);
      await expectNoHorizontalOverflow(page);
      const populatedSnapshot = `desk-populated-state-${theme}-${viewport.width}.png`;
      if (viewport.width !== 320 && hasVisualBaseline(populatedSnapshot)) {
        await expect(page).toHaveScreenshot(populatedSnapshot, { animations: "disabled", caret: "hide", maxDiffPixelRatio: 0.005 });
      }
    }
  }
  await page.goto(`${baseURL}/test/empty-state?theme=light&populated=0&shell=1`);
  const lightCanvas = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--surface-canvas").trim());
  await page.goto(`${baseURL}/test/empty-state?theme=dark&populated=0&shell=1`);
  const darkCanvas = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--surface-canvas").trim());
  expect(darkCanvas).not.toBe(lightCanvas);
});

test("structured empty state reflows at an honest 200 percent zoom equivalent", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the explicit zoom contract once.");
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(`${baseURL}/test/empty-state?theme=light&populated=0&shell=1`);
  const before = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    innerHeight: window.innerHeight,
    clientWidth: document.documentElement.clientWidth,
    visualViewportWidth: window.visualViewport?.width ?? 0,
    maxWidth200: window.matchMedia("(max-width: 200px)").matches,
  }));
  const cdp = await page.context().newCDPSession(page);
  // A 200% browser zoom gives a 375px physical viewport roughly 188 CSS px
  // wide. Device metrics emulation applies that layout viewport directly;
  // page-scale-factor alone changes compositor scale without reflowing CSS.
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: 188,
    height: 406,
    deviceScaleFactor: 1,
    mobile: false,
    screenWidth: 375,
    screenHeight: 812,
  });
  await expect.poll(() => page.evaluate(() => window.innerWidth)).toBe(188);
  const after = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    innerHeight: window.innerHeight,
    clientWidth: document.documentElement.clientWidth,
    visualViewportWidth: window.visualViewport?.width ?? 0,
    maxWidth200: window.matchMedia("(max-width: 200px)").matches,
  }));
  expect(after.innerWidth).toBeLessThan(before.innerWidth);
  expect(after.innerHeight).toBeLessThan(before.innerHeight);
  expect(after.clientWidth).toBe(after.innerWidth);
  expect(after.visualViewportWidth).toBe(after.innerWidth);
  expect(after.maxWidth200).toBe(true);
  expect(before.maxWidth200).toBe(false);
  const overflow = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    clientWidth: document.documentElement.clientWidth,
    documentScrollWidth: document.documentElement.scrollWidth,
    bodyScrollWidth: document.body.scrollWidth,
  }));
  expect(overflow.documentScrollWidth, JSON.stringify(overflow)).toBeLessThanOrEqual(overflow.innerWidth);
  expect(overflow.bodyScrollWidth, JSON.stringify(overflow)).toBeLessThanOrEqual(overflow.innerWidth);
  const initial = await page.evaluate(() => {
    const rect = (selector) => {
      const element = document.querySelector(selector);
      if (!element) {
        return null;
      }
      const bounds = element.getBoundingClientRect();
      return {
        left: bounds.left,
        right: bounds.right,
        top: bounds.top,
        bottom: bounds.bottom,
        width: bounds.width,
        height: bounds.height,
      };
    };
    const main = document.querySelector("main");
    const scrollingElement = document.scrollingElement;
    return {
      mainScrollTop: main?.scrollTop ?? 0,
      documentScrollTop: scrollingElement?.scrollTop ?? 0,
      windowScrollY: window.scrollY,
      empty: rect(".waffle-empty-state"),
      image: rect(".waffle-empty-state img"),
      primary: rect(".waffle-empty-state .action-primary"),
      navigation: rect(".desk-navigation"),
      viewport: { width: window.innerWidth, height: window.innerHeight },
    };
  });
  expect(initial.mainScrollTop).toBe(0);
  expect(initial.documentScrollTop).toBe(0);
  expect(initial.windowScrollY).toBe(0);
  expect(initial.empty).not.toBeNull();
  expect(initial.primary).not.toBeNull();
  expect(initial.navigation).not.toBeNull();
  expect(initial.primary.left).toBeGreaterThanOrEqual(0);
  expect(initial.primary.right).toBeLessThanOrEqual(initial.viewport.width);
  expect(initial.primary.top).toBeGreaterThanOrEqual(0);
  expect(initial.primary.bottom, JSON.stringify(initial)).toBeLessThanOrEqual(initial.navigation.top + 1);
  expect(initial.image).not.toBeNull();
  expect(initial.image.width, JSON.stringify(initial)).toBeGreaterThan(0);
  expect(initial.image.height, JSON.stringify(initial)).toBeGreaterThan(0);
  expect(initial.image.left, JSON.stringify(initial)).toBeGreaterThanOrEqual(0);
  expect(initial.image.right, JSON.stringify(initial)).toBeLessThanOrEqual(initial.viewport.width);
  const metrics = await page.locator(".waffle-empty-state").evaluate((empty) => {
    const region = empty.getBoundingClientRect();
    const image = empty.querySelector("img").getBoundingClientRect();
    const primary = empty.querySelector(".action-primary").getBoundingClientRect();
    const nav = document.querySelector(".desk-navigation").getBoundingClientRect();
    return {
      region: { left: region.left, right: region.right },
      image: { width: image.width, height: image.height, right: image.right },
      primary: { left: primary.left, top: primary.top, bottom: primary.bottom, right: primary.right },
      nav: { top: nav.top, bottom: nav.bottom },
      viewport: { width: window.innerWidth, height: window.innerHeight },
      scrollWidth: document.documentElement.scrollWidth,
    };
  });
  expect(metrics.image.width).toBeLessThanOrEqual(320);
  expect(metrics.image.height).toBeLessThanOrEqual(320);
  expect(metrics.region.left).toBeGreaterThanOrEqual(0);
  expect(metrics.region.right).toBeLessThanOrEqual(metrics.viewport.width);
  expect(metrics.primary.left).toBeGreaterThanOrEqual(0);
  expect(metrics.primary.right).toBeLessThanOrEqual(metrics.viewport.width);
  expect(metrics.primary.top).toBeGreaterThanOrEqual(0);
  expect(metrics.primary.bottom).toBeLessThanOrEqual(metrics.nav.top + 1);
  expect(metrics.scrollWidth).toBeLessThanOrEqual(metrics.viewport.width);
});

test("compact Today opens at the top while preserving message focus", async ({ page }) => {
  test.skip(!["tablet", "mobile", "narrow"].includes(test.info().project.name), "Run the compact initial-state contract on tablet and mobile widths.");
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const metrics = await page.evaluate(() => {
    const rect = (selector) => {
      const element = document.querySelector(selector);
      if (!element) {
        return null;
      }
      const bounds = element.getBoundingClientRect();
      return { top: bounds.top, bottom: bounds.bottom, left: bounds.left, right: bounds.right };
    };
    const main = document.querySelector("main");
    const columns = document.querySelector(".today-columns");
    return {
      activeElement: document.activeElement?.id || "",
      mainScrollTop: main?.scrollTop ?? 0,
      columnsScrollTop: columns?.scrollTop ?? 0,
      main: rect("main"),
      today: rect(".today"),
      columns: rect(".today-columns"),
      conversation: rect(".conversation"),
      transcript: rect("#desk-transcript"),
      composer: rect("#desk-composer"),
      message: rect("#desk-message"),
      actions: rect(".composer-actions"),
      setup: rect("#desk-setup-banner"),
      context: rect("#desk-canvas-drawer"),
      contextHidden: document.querySelector("#desk-canvas-drawer")?.hidden ?? true,
      viewportHeight: window.innerHeight,
    };
  });
  expect(metrics.activeElement).toBe("desk-message");
  expect(metrics.mainScrollTop).toBe(0);
  await expectVisibleComposerActionsClearOfNavigation(page, `${test.info().project.name} initial composer`);
  expect(metrics.columnsScrollTop).toBe(0);
  expect(metrics.columns).not.toBeNull();
  expect(metrics.conversation).not.toBeNull();
  expect(metrics.transcript).not.toBeNull();
  expect(metrics.contextHidden).toBe(true);
  expect(metrics.conversation.top).toBeGreaterThanOrEqual(metrics.columns.top - 1);
  expect(metrics.transcript.top).toBeGreaterThanOrEqual(metrics.conversation.top - 1);
  expect(metrics.today.top).toBeGreaterThanOrEqual(metrics.main.top - 1);
  expect(metrics.today.bottom).toBeGreaterThan(metrics.columns.top);
  expect(metrics.columns.top).toBeGreaterThanOrEqual(0);
  expect(metrics.columns.bottom).toBeGreaterThan(metrics.columns.top);
});

// Visual baselines (#469): the five destinations at every configured width,
// captured on a fresh fixture so the renders are deterministic. Baselines are
// intentional and reviewable in CI artifacts; regenerate with
// `npx playwright test --update-snapshots` after a deliberate visual change.
const visualSections = [
  ["today", "today", ".today", "#desk-phase:has-text('Ready')"],
  ["tasks", "tasks", ".tasks-board", "#tasks-list article, #tasks-list .waffle-fragment-empty"],
  ["workspaces", "workspaces", ".workspaces-grid", ".workspaces-grid article, .workspaces-grid .waffle-fragment-empty"],
  ["memory", "memory", ".memory-search-panel", "#memory-status"],
  ["capabilities", "capabilities", "#desk-capabilities", ""],
];

for (const [name, section, settled] of visualSections) {
  test(`visual baseline ${name} renders scannable and unclipped at every width`, async ({ page }) => {
    const baselinePath = path.join(
      testsDir,
      "desk.spec.mjs-snapshots",
      `desk-visual-${name}-${test.info().project.name}-${process.platform}.png`,
    );
    if (!existsSync(baselinePath) && process.env.WAFFLE_VISUAL_BASELINES !== "1") {
      test.skip(
        `no committed ${process.platform} baseline for ${name}; set WAFFLE_VISUAL_BASELINES=1 with --update-snapshots to generate`,
      );
    }
    await page.goto(deskURL(section));
    if (section === "today") {
      await expect(page.locator("#desk-phase")).toHaveText("Ready");
    } else if (section === "capabilities") {
      // Panels are display:none until their tab is targeted; open Models.
      await openCapabilityTab(page, "Models");
      await expect(page.locator("#capability-models .capability-card").first()).toBeVisible({
        timeout: 10_000,
      });
    } else {
      // Wait for the async fragment to settle rather than a fixed delay.
      const settleSelector = visualSections.find(([name]) => name === section)[3];
      await expect(page.locator(settleSelector).first()).toBeVisible({ timeout: 10_000 });
    }
    await expect(page).toHaveScreenshot(`desk-visual-${name}-${test.info().project.name}.png`, {
      animations: "disabled",
      caret: "hide",
      // Cross-platform font metrics shift a little text wrapping; keep the
      // threshold tight enough to catch layout regressions but not platform
      // font variance.
      maxDiffPixelRatio: 0.005,
    });
  });
}

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
      "default-src 'self'; img-src 'self' data: blob:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
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
  await page.getByRole("button", { name: /Session and files/i }).click();
  await expect(page.locator("#desk-canvas-drawer")).toBeVisible();
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

test("Tasks guided schedule form creates, edits, and reports filter state", async ({ page }) => {
  await page.goto(deskURL("tasks"));
  await page.getByRole("button", { name: "New schedule", exact: true }).click();
  const form = page.locator("#task-schedule-form");
  await expect(form.getByRole("button", { name: "Cancel", exact: true })).toBeVisible();

  // Guided controls describe the cadence in plain language and show a live
  // human summary with the next run.
  await form.getByLabel("Name").fill("Fixture schedule");
  await form.getByLabel("Prompt").fill("Review the fixture queue");
  await expect(form.locator("#task-schedule-summary")).toContainText("Every weekday at 09:00", {
    timeout: 10_000,
  });
  await expect(form.locator("#task-schedule-summary")).toContainText("next");
  // Configured choices: the reviewer profile is offered.
  await expect(form.locator("#task-schedule-profile")).toContainText("reviewer", {
    timeout: 10_000,
  });
  await form.locator("#task-schedule-profile").selectOption("reviewer");

  const created = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/tasks/schedules") &&
      response.request().method() === "POST",
  );
  await form.getByRole("button", { name: "Create schedule", exact: true }).click();
  await created;

  const card = page.locator("[data-task-id='job-added']");
  await expect(card).toContainText("Fixture schedule");
  // The card repeats the human schedule rather than raw cron.
  await expect(card).toContainText("Every weekday at 09:00");
  await expect(card).toContainText("Next run");

  await card.getByRole("button", { name: "Edit schedule", exact: true }).click();
  await expect(form.getByLabel("Name")).toHaveValue("Fixture schedule");
  // Guided controls re-derive from the stored cron.
  await expect(form.getByLabel("Cadence")).toHaveValue("weekdays");
  await expect(form.getByLabel("Time")).toHaveValue("09:00");
  await expect(form.locator("#task-schedule-profile")).toHaveValue("reviewer");
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

test("Tasks schedule advanced cron validates inline and rejects bad expressions", async ({ page }) => {
  await page.goto(deskURL("tasks"));
  await page.getByRole("button", { name: "New schedule", exact: true }).click();
  const form = page.locator("#task-schedule-form");
  await form.getByLabel("Name").fill("Invalid fixture schedule");
  await form.getByLabel("Prompt").fill("This must not be saved");
  // Advanced mode: raw cron with help.
  await form.locator("#task-schedule-advanced summary").click();
  await expect(form.locator("#task-schedule-advanced")).toContainText("Five fields");
  await form.getByLabel("Cron schedule").fill("not-a-cron");
  await expect(form.locator("#task-schedule-field-errors")).toContainText("cron", {
    timeout: 10_000,
  });
  await expect(form.locator("#task-schedule-field-errors")).toContainText("not valid");
  const invalid = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/tasks/schedules") &&
      response.request().method() === "POST" &&
      response.status() === 422,
  );
  await form.getByRole("button", { name: "Create schedule", exact: true }).click();
  await invalid;
  allowDiagnostics("422", "Response Status Error Code 422");
  await expect(form.locator("[data-waffle-error='true']")).toContainText("schedule definition is invalid");
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

test("a Today prompt hands off into the reviewed schedule editor", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the handoff flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const message = page.getByLabel("Message Waffle");
  await message.fill("Summarize the release queue every morning");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".user-message .message-body")).toHaveText(
    "Summarize the release queue every morning",
  );

  // The completed user prompt exposes Create schedule; the handoff never puts
  // the prompt text in the URL.
  const userMessage = page.locator(".user-message").last();
  await userMessage.getByRole("button", { name: "Create a schedule from this prompt" }).click();
  await expect(page).toHaveURL(/section=tasks/);
  expect(decodeURIComponent(page.url())).not.toContain("Summarize the release queue");

  // Tasks opens the shared guided editor prefilled with exactly that prompt.
  const form = page.locator("#task-schedule-form");
  await expect(form).toBeVisible();
  await expect(form.getByLabel("Prompt")).toHaveValue("Summarize the release queue every morning");
  await expect(form.locator("#task-schedule-summary")).toContainText("Every weekday at 09:00");

  // The handoff state is consumed: reopening the dialog starts clean.
  await form.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("button", { name: "New schedule", exact: true }).click();
  await expect(form.getByLabel("Prompt")).toHaveValue("");
});

test("completed responses expose a keyboard-friendly read-aloud control", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the read-aloud flow once.");
  await page.addInitScript(() => {
    class FakeUtterance {
      constructor(text) {
        this.text = text;
        this.onend = null;
        this.onerror = null;
        this.voice = null;
      }
    }
    const synth = {
      speaking: false,
      getVoices() {
        return [{ default: true, name: "Test", lang: "en-US" }];
      },
      addEventListener() {},
      speak() {
        this.speaking = true;
      },
      cancel() {
        this.speaking = false;
      },
    };
    Object.defineProperty(window, "speechSynthesis", { configurable: true, value: synth });
    window.SpeechSynthesisUtterance = FakeUtterance;
  });
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const message = page.getByLabel("Message Waffle");
  await message.fill("Speak to me");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const reply = page.locator(".waffle-message").last();
  const readButton = reply.locator(".message-read");
  await expect(readButton).toBeVisible();
  await expect(readButton).toHaveAttribute("aria-label", "Read this response aloud");
  // Activation changes to a clear Stop state; Copy remains available.
  await readButton.focus();
  await page.keyboard.press("Enter");
  await expect(readButton).toHaveText("Stop");
  await expect(reply.getByRole("button", { name: "Copy message" })).toBeVisible();
  await readButton.click();
  await expect(readButton).toHaveText("Read aloud");
  await expect(readButton).toHaveAttribute("aria-pressed", "false");
});

test("composer exposes a privacy-first dictation control with clear states", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the dictation flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const dictate = page.locator("#desk-dictate");
  const supported = await page.evaluate(
    () => Boolean(window.SpeechRecognition || window.webkitSpeechRecognition),
  );
  await expect(dictate).toBeVisible();
  // Disclosure lives in visible hint text, wired with aria-describedby.
  await expect(dictate).toHaveAttribute("aria-describedby", "desk-dictate-hint");
  await expect(page.locator("#desk-dictate-hint")).toContainText(
    "Browser speech may process audio off-device. It is not sent to Waffle.",
  );
  if (supported) {
    await expect(dictate).toBeEnabled();
    await dictate.click();
    await expect(dictate).toHaveText("Stop dictation");
    await expect(dictate).toHaveAttribute("aria-pressed", "true");
    await page.keyboard.press("Escape");
    await expect(dictate).toHaveText("Dictate");
    await expect(dictate).toHaveAttribute("aria-pressed", "false");
    await expect(page.getByLabel("Message Waffle")).toBeFocused();
  } else {
    await expect(dictate).toBeDisabled();
  }
});

test("Today model choices explain roles and operator descriptions", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the model picker flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const detail = page.locator("#desk-model-detail");
  await expect(detail).toContainText("Waffle-wide default", { timeout: 10_000 });
  await expect(detail).toContainText("Utility model");
  await expect(detail).toContainText("fixture → primary-model");
  await expect(detail).toContainText("Everyday reasoning and tool use");
  // Switching choices updates the explanation; the alias stays prominent in
  // the option itself.
  await page.getByLabel("Session model").selectOption("local");
  await expect(detail).toContainText("fixture → local-model");
  await expect(detail).toContainText("Fast local drafts");
});

test("owner can export the visible transcript with their live lease", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the export flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const message = page.getByLabel("Message Waffle");
  await message.fill("Summarize the release queue");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".waffle-message .message-body")).toHaveText("Fixture reply");

  await page.getByRole("button", { name: "Session and files", exact: true }).click();
  await expect(page.locator("#desk-canvas-tab-session")).toHaveAttribute("aria-selected", "true");
  const downloadPromise = page.waitForEvent("download");
  await page.getByRole("button", { name: "Export", exact: true }).click();
  const download = await downloadPromise;
  const path = await download.path();
  const content = await (await import("node:fs/promises")).readFile(path, "utf8");
  expect(content).toContain("Summarize the release queue");
  expect(content).toContain("Fixture reply");
  expect(content).not.toContain("secret");
  expect(download.suggestedFilename()).toMatch(/\.md$/);
});

test("temporary conversations are offered before the first message and marked live", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the temporary flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.getByRole("button", { name: "Session and files", exact: true }).click();
  await expect(page.locator("#desk-canvas-tab-session")).toHaveAttribute("aria-selected", "true");
  const toggle = page.locator("#desk-temporary");
  await expect(toggle).toBeVisible();
  // Switching the option before the first message reopens the empty session
  // as temporary and marks it live.
  await toggle.check();
  await expect(page.locator("#desk-session-title")).toHaveText("Temporary conversation");
  await expect(page.locator("#desk-temporary-badge")).toBeVisible();
  await expect(page.locator("#desk-temporary-badge")).toHaveText("Temporary — nothing is saved");
  // A message still works end-to-end in the temporary conversation.
  const message = page.getByLabel("Message Waffle");
  await message.fill("Transient question");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".waffle-message .message-body")).toHaveText("Fixture reply");
});

test("Today attaches images securely, previews them, and sends them with the turn", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the attachment flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");

  // A real 1x1 PNG in a temp file.
  const { writeFileSync, mkdtempSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const { join } = await import("node:path");
  const dir = mkdtempSync(join(tmpdir(), "waffle-attach-"));
  const png = Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
    "base64",
  );
  const filePath = join(dir, "shot.png");
  writeFileSync(filePath, png);

  await page.locator("#desk-attach").setInputFiles(filePath);
  await expect(page.locator(".attachment-chip")).toBeVisible();
  await expect(page.locator(".attachment-chip")).toContainText("shot.png");

  const turn = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/chat/turn") &&
      response.request().method() === "POST",
  );
  const message = page.getByLabel("Message Waffle");
  await message.fill("Look at this screenshot");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const response = await turn;
  expect(response.status()).toBe(200);
  const body = response.request().postDataJSON();
  expect(body.attachments).toHaveLength(1);
  expect(body.attachments[0].name).toBe("shot.png");
  expect(body.attachments[0].media_type).toBe("image/png");
  await expect(page.locator(".waffle-message .message-body")).toHaveText("Fixture reply");
  await expect(page.locator(".attachment-chip")).toHaveCount(0);
});

test("command palette opens everywhere, searches, and invokes existing actions", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the palette flow once.");
  // Navigating between sections closes the chat owner on pagehide; the next
  // Today load reattaches, finds the closed client, and silently opens fresh.
  // The resulting 404 is the expected recovery path, not a regression.
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const palette = page.locator("#command-palette");
  const search = page.getByLabel("Search commands");

  // The visible button opens the palette; Escape closes it.
  await page.getByRole("button", { name: /Commands Ctrl K/ }).click();
  await expect(palette).toBeVisible();
  await expect(search).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(palette).toBeHidden();

  // Searching a navigation destination and selecting it navigates.
  await page.getByRole("button", { name: /Commands Ctrl K/ }).click();
  await search.fill("tasks");
  await expect(palette.locator(".palette-item").first()).toContainText("Go to Tasks");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.getByRole("button", { name: /Commands Ctrl K/ }).click();
  await search.fill("tasks");
  await palette.locator(".palette-item").first().click();
  await expect(page).toHaveURL(/section=tasks/);
  await expect(palette).toBeHidden();

  // The palette is available on every section.
  await page.getByRole("button", { name: /Commands Ctrl K/ }).click();
  await expect(palette).toBeVisible();
  await expect(search).toBeFocused();
  await search.fill("memory");
  await palette.locator(".palette-item").first().click();
  await expect(page).toHaveURL(/section=memory/);

  // The shortcut help lists discoverable keys without navigating.
  await page.getByRole("button", { name: /Commands Ctrl K/ }).click();
  await page.getByRole("button", { name: "Keyboard shortcuts", exact: true }).click();
  await expect(palette.locator(".palette-results")).toContainText("Ctrl/Cmd + K");
  await page.keyboard.press("Escape");
  await expect(palette).toBeHidden();
});

test("command palette includes canonical chat commands on Today", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the command palette once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.getByRole("button", { name: /Commands Ctrl K/ }).click();
  const palette = page.locator("#command-palette");
  await expect(palette).toBeVisible();
  const search = page.getByLabel("Search commands");
  await search.fill("/new");
  await expect(palette.locator(".palette-item").first()).toContainText("/new");
  await palette.locator(".palette-item").first().click();
  // The canonical /new command path runs with its confirmation preserved.
  await expect(page).toHaveURL(/section=today/);
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
});

test("per-turn task and reasoning modes are honest and persist on the turn", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the turn mode flow once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  // Plain-language guidance accompanies the mode controls.
  await page.getByLabel("Task mode").selectOption("deep");
  await page.getByLabel("Reasoning effort").selectOption("high");
  const turn = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/chat/turn") &&
      response.request().method() === "POST",
  );
  const message = page.getByLabel("Message Waffle");
  await message.fill("Work this out");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const response = await turn;
  expect(response.status()).toBe(200);
  const body = response.request().postDataJSON();
  expect(body.task_mode).toBe("deep");
  expect(body.reasoning_effort).toBe("high");
  await expect(page.locator(".waffle-message .message-body")).toHaveText("Fixture reply");
});

test("the shared visual system keeps hierarchy, density, and focus readable", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the visual system once.");
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.getByRole("button", { name: "Session and files", exact: true }).click();
  // The conversation is the strongest surface on Today.
  const tokens = await page.evaluate(() => {
    const root = getComputedStyle(document.documentElement);
    const conversation = getComputedStyle(document.querySelector(".conversation"));
    const context = getComputedStyle(document.querySelector("#desk-canvas-drawer"));
    const send = getComputedStyle(document.querySelector("#desk-send"));
    return {
      tokenRadius: root.getPropertyValue("--radius-card").trim(),
      conversationRadius: conversation.borderRadius,
      contextBackground: context.backgroundColor,
      contextRadius: context.borderRadius,
      sendBackground: send.backgroundColor,
      focusRing: root.getPropertyValue("--focus-ring").trim(),
    };
  });
  expect(tokens.tokenRadius).not.toBe("");
  expect(tokens.conversationRadius).not.toBe("0px");
  expect(tokens.focusRing).toContain("#9d421f");
  // The primary send is the orange personality button; the canvas is a
  // quieter paper surface.
  expect(tokens.sendBackground).toBe("rgb(221, 113, 40)");
  expect(tokens.contextBackground).toBe("rgb(255, 250, 240)");
  expect(tokens.contextRadius).not.toBe("0px");
  // Keyboard focus is visibly ringed.
  await page.getByLabel("Message Waffle").focus();
  const ring = await page.evaluate(() =>
    getComputedStyle(document.querySelector("#desk-message")).boxShadow,
  );
  expect(ring).toMatch(/rgb\(157, 66, 31/);
});

test("theme boot paints each case before app.js can run", async ({ browser }) => {
  const project = test.info().project.name;
  test.skip(!["desktop", "mobile"].includes(project), "Run the theme contract at desktop and mobile sizes.");

  const cases = [
    { stored: "light", colorScheme: "light", theme: "light", canvas: "rgb(244, 237, 223)", rail: "rgb(33, 29, 25)" },
    { stored: "system", colorScheme: "dark", theme: "dark", canvas: "rgb(23, 19, 15)", rail: "rgb(15, 13, 11)" },
    { stored: "dark", colorScheme: "light", theme: "dark", canvas: "rgb(23, 19, 15)", rail: "rgb(15, 13, 11)" },
  ];

  for (const themeCase of cases) {
    const context = await browser.newContext({
      colorScheme: themeCase.colorScheme,
      viewport: project === "desktop"
        ? { width: 1414, height: 786 }
        : { width: 375, height: 812 },
    });
    const casePage = await context.newPage();
    const diagnostics = [];
    const requests = [];
    casePage.on("console", (message) => {
      if (message.type() === "error" || message.type() === "warning") {
        const text = message.text();
        if (!text.includes("favicon") && !text.includes("net::ERR_ABORTED")) {
          diagnostics.push(`${message.type()}: ${text}`);
        }
      }
    });
    casePage.on("pageerror", (error) => diagnostics.push(`pageerror: ${String(error)}`));
    casePage.on("request", (request) => {
      const url = request.url();
      if (url.includes("theme-boot.js") || url.includes("app.css") || url.includes("app.js")) {
        requests.push(url);
      }
    });
    await casePage.route("**/desk/assets/app.js*", (route) => route.fulfill({
      status: 200,
      contentType: "text/javascript",
      body: "",
    }));
    await casePage.addInitScript(({ stored }) => {
      localStorage.setItem("waffle.desk.theme", stored);
    }, { stored: themeCase.stored });
    await casePage.goto(deskURL("tasks"));

    const rendered = await casePage.evaluate(() => {
      const html = getComputedStyle(document.documentElement);
      const rail = getComputedStyle(document.querySelector(".desk-navigation"));
      return {
        theme: document.documentElement.dataset.theme,
        preference: document.documentElement.dataset.themePreference,
        canvas: html.backgroundColor,
        rail: rail.backgroundColor,
      };
    });
    expect(rendered).toEqual({
      theme: themeCase.theme,
      preference: themeCase.stored,
      canvas: themeCase.canvas,
      rail: themeCase.rail,
    });
    expect(requests.findIndex((url) => url.includes("theme-boot.js"))).toBeLessThan(
      requests.findIndex((url) => url.includes("app.css")),
    );
    expect(requests.filter((url) => url.includes("app.js"))).toHaveLength(1);
    expect(diagnostics).toEqual([]);
    await context.close();
  }
});

test("theme preference persists through reload and section navigation", async ({ page }) => {
  const project = test.info().project.name;
  test.skip(!["desktop", "mobile"].includes(project), "Run theme persistence at desktop and mobile sizes.");
  await page.setViewportSize(
    project === "desktop" ? { width: 1414, height: 786 } : { width: 375, height: 812 },
  );
  await page.goto(deskURL("tasks"));
  await page.getByLabel("Theme").selectOption("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme-preference", "dark");
  await page.reload();
  await expect(page.getByLabel("Theme")).toHaveValue("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.goto(deskURL("memory"));
  await expect(page.getByLabel("Theme")).toHaveValue("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.getByLabel("Theme").selectOption("light");
  await page.goto(deskURL("workspaces"));
  await expect(page.getByLabel("Theme")).toHaveValue("light");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
});

test("light code and dark destructive surfaces meet computed contrast", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the focused contrast regression once.");
  await page.addInitScript(() => localStorage.setItem("waffle.desk.theme", "light"));
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.getByLabel("Message Waffle").fill("markdown");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const lightCode = await page.locator(".code-block").evaluate((element) => {
    const style = getComputedStyle(element);
    return { background: style.backgroundColor, color: style.color };
  });
  const lightCodeContrast = contrastRatio(lightCode.color, lightCode.background);

  await page.goto(deskURL("workspaces"));
  await page.getByLabel("Theme").selectOption("dark");
  const clean = page.locator("[data-workspace-id='workspace-clean']");
  await expect(clean).toBeVisible();
  await clean.getByRole("button", { name: "Review close", exact: true }).click();
  await expect(page.locator("#workspace-close-dialog")).toBeVisible();
  const darkDanger = await page.locator("#workspace-close-confirm").evaluate((element) => {
    const style = getComputedStyle(element);
    return { background: style.backgroundColor, color: style.color };
  });
  const darkDangerContrast = contrastRatio(darkDanger.color, darkDanger.background);

  expect.soft(lightCode).toEqual({ background: "rgb(33, 29, 25)", color: "rgb(244, 237, 223)" });
  expect.soft(lightCodeContrast, `light code contrast: ${lightCodeContrast.toFixed(2)}:1`).toBeGreaterThanOrEqual(4.5);
  expect.soft(darkDanger).toEqual({ background: "rgb(243, 161, 153)", color: "rgb(33, 29, 25)" });
  expect.soft(darkDangerContrast, `dark destructive contrast: ${darkDangerContrast.toFixed(2)}:1`).toBeGreaterThanOrEqual(4.5);
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
});

test("Evening role tokens keep rail, code, actions, and accents readable", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run semantic Evening surface checks once.");
  await page.addInitScript(() => localStorage.setItem("waffle.desk.theme", "dark"));
  await page.emulateMedia({ colorScheme: "dark" });
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.getByLabel("Message Waffle").fill("markdown");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".code-block pre")).toBeVisible();
  const codeSurface = await page.locator(".code-block").evaluate((element) => {
    const style = getComputedStyle(element);
    return { background: style.backgroundColor, color: style.color };
  });
  expect(codeSurface).toEqual({ background: "rgb(15, 13, 11)", color: "rgb(242, 233, 220)" });

  await page.goto(deskURL("workspaces"));
  await expect(page.locator(".workspace-card .workspace-primary").first()).toBeVisible();
  const focusWithKeyboard = async (locator) => {
    await page.evaluate(() => document.activeElement?.blur());
    for (let index = 0; index < 40; index += 1) {
      if (await locator.evaluate((element) => element === document.activeElement)) {
        return;
      }
      await page.keyboard.press("Tab");
    }
    throw new Error(`could not keyboard-focus ${await locator.getAttribute("id")}`);
  };
  const themeControl = page.locator("#desk-theme");
  await themeControl.selectOption("light");
  const lightRail = await page.evaluate(() => {
    const rail = getComputedStyle(document.querySelector(".desk-navigation"));
    const status = document.querySelector("#rail-status");
    status.dataset.connectionState = "disconnected";
    const danger = getComputedStyle(document.querySelector("#rail-connection"));
    return {
      background: rail.backgroundColor,
      danger: danger.color,
    };
  });
  await focusWithKeyboard(themeControl);
  const lightRailFocusShadow = await themeControl.evaluate((element) => getComputedStyle(element).boxShadow);
  const lightRailFocusContrast = contrastRatio(shadowColor(lightRailFocusShadow), lightRail.background);
  await themeControl.selectOption("dark");
  await focusWithKeyboard(themeControl);
  const railFocusShadow = await themeControl.evaluate((element) => getComputedStyle(element).boxShadow);
  const selectedLink = page.locator('.section-links a[aria-current="page"]');
  await focusWithKeyboard(selectedLink);
  const selectedFocusShadow = await selectedLink.evaluate((element) => getComputedStyle(element).boxShadow);
  const workspacePrimary = page.locator(".workspace-card .workspace-primary").first();
  await focusWithKeyboard(workspacePrimary);
  await expect.poll(() => workspacePrimary.evaluate((element) => getComputedStyle(element).boxShadow)).toContain("rgb(245, 197, 121)");
  const workspaceFocusShadow = await workspacePrimary.evaluate((element) => getComputedStyle(element).boxShadow);
  const surfaces = await page.evaluate(({ railFocusShadow, selectedFocusShadow, workspaceFocusShadow }) => {
    const nav = document.querySelector(".desk-navigation");
    const railStatus = document.querySelector("#rail-status");
    railStatus.dataset.connectionState = "disconnected";
    const selected = document.querySelector('.section-links a[aria-current="page"]');
    const workspacePrimary = document.querySelector(".workspace-card .workspace-primary");
    const navStyle = getComputedStyle(nav);
    const dangerStyle = getComputedStyle(document.querySelector("#rail-connection"));
    const workspaceStyle = getComputedStyle(workspacePrimary);
    const workspaceCard = getComputedStyle(workspacePrimary.closest(".workspace-card"));
    return {
      railBackground: navStyle.backgroundColor,
      dangerColor: dangerStyle.color,
      railFocusShadow,
      selectedBackground: getComputedStyle(selected).backgroundColor,
      selectedFocusShadow,
      primaryBackground: workspaceStyle.backgroundColor,
      primaryColor: workspaceStyle.color,
      workspaceFocusShadow,
      workspaceCardBackground: workspaceCard.backgroundColor,
      brandColor: getComputedStyle(document.querySelector(".brand")).color,
      paletteColor: getComputedStyle(document.querySelector("#palette-open")).color,
      paletteKbdColor: getComputedStyle(document.querySelector("#palette-open kbd")).color,
    };
  }, { railFocusShadow, selectedFocusShadow, workspaceFocusShadow });
  const lightRailDangerContrast = contrastRatio(lightRail.danger, lightRail.background);
  const darkRailDangerContrast = contrastRatio(surfaces.dangerColor, surfaces.railBackground);
  const darkRailFocusContrast = contrastRatio(shadowColor(surfaces.railFocusShadow), surfaces.railBackground);
  const workspaceFocusContrast = contrastRatio(shadowColor(surfaces.workspaceFocusShadow), surfaces.workspaceCardBackground);
  expect(lightRail.background).toBe("rgb(33, 29, 25)");
  expect(lightRail.danger).toBe("rgb(243, 161, 153)");
  expect(lightRailDangerContrast).toBeGreaterThanOrEqual(4.5);
  expect(lightRailFocusShadow).toContain("rgb(221, 113, 40)");
  expect(lightRailFocusContrast).toBeGreaterThanOrEqual(3);
  expect(surfaces.railBackground).toBe("rgb(15, 13, 11)");
  expect(darkRailDangerContrast).toBeGreaterThanOrEqual(4.5);
  expect(darkRailFocusContrast).toBeGreaterThanOrEqual(3);
  expect(workspaceFocusContrast).toBeGreaterThanOrEqual(3);
  expect(surfaces.selectedBackground).toBe("rgb(221, 113, 40)");
  expect(surfaces.primaryBackground).toBe("rgb(15, 13, 11)");
  expect(surfaces.primaryColor).toBe("rgb(242, 233, 220)");
  expect(surfaces.brandColor).toBe("rgb(245, 197, 121)");
  expect(surfaces.paletteColor).toBe("rgb(245, 197, 121)");
  expect(surfaces.paletteKbdColor).toBe("rgb(245, 197, 121)");
  expect(surfaces.railFocusShadow).toContain("rgb(245, 197, 121)");
  expect(surfaces.selectedFocusShadow).toContain("rgb(245, 197, 121)");
  expect(surfaces.workspaceFocusShadow).toContain("rgb(245, 197, 121)");
});

test("Today retries a rejected turn with the same idempotency key", async ({ page }) => {
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const message = page.getByLabel("Message Waffle");
  await message.fill("Retry me");

  // Force the fixture to fail the turn once, then restore it.
  await page.request.post(`${baseURL}/api/v1/desk/test/turn-fail?on=1`);
  try {
    await page.getByRole("button", { name: "Send message", exact: true }).click();
    const retry = page.locator(".retry-button");
    await expect(retry).toBeVisible();
    // The failure state is explicit, not a silent hang.
    await expect(page.locator("#desk-composer-status")).toContainText("could not be completed");
    allowDiagnostics("400", "Bad Request");
    // Restore the fixture so the retry succeeds, then retry in place.
    await page.request.post(`${baseURL}/api/v1/desk/test/turn-fail?on=0`);
    const second = page.waitForResponse(
      (response) =>
        response.url().endsWith("/api/v1/desk/chat/turn") &&
        response.request().method() === "POST" &&
        response.status() === 200,
    );
    await retry.click();
    await second;
    await expect(page.locator(".waffle-message .message-body")).toHaveText("Fixture reply");
    await expect(page.locator(".retry-button")).toHaveCount(0);
  } finally {
    try {
      await page.request.post(`${baseURL}/api/v1/desk/test/turn-fail?on=0`, {
        timeout: 5_000,
      });
    } catch {
      // Best effort: the fixture still serves later tests.
    }
  }
});

test("Today plain Enter sends and streaming markdown renders long content", async ({ page }) => {
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const message = page.getByLabel("Message Waffle");

  // Plain Enter (no modifiers) sends the turn (#469).
  const turn = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/desk/chat/turn") &&
      response.request().method() === "POST",
  );
  await message.fill("markdown please");
  await message.press("Enter");
  const response = await turn;
  expect(response.status()).toBe(200);

  // The streamed markdown settles into headings, code, and a table.
  await expect(page.locator(".waffle-message h2")).toContainText("Fixture markdown");
  await expect(page.locator(".waffle-message code").filter({ hasText: "mise" })).toBeVisible();
  await expect(page.locator(".waffle-message table")).toContainText("figma");
  await expect(page.locator(".waffle-message pre code")).toContainText("fmt.Println");
  // The composer cleared for the next turn.
  await expect(message).toHaveValue("");
});


test("Today slash menu completes commands and skills", async ({ page }) => {
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const message = page.getByLabel("Message Waffle");
  await message.fill("/ne");
  await expect(page.locator("#desk-slash-menu")).toBeVisible();
  await expect(page.locator("#desk-slash-menu")).toContainText("/new");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  // Tab completes the selection; the message keeps the prefix.
  await message.fill("/ne");
  await page.keyboard.press("Tab");
  await expect(message).toHaveValue("/new ");
  await page.keyboard.press("Escape");
  await expect(page.locator("#desk-slash-menu")).toBeHidden();
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

  await page.getByRole("button", { name: "Session and files", exact: true }).click();
  await page.getByRole("tab", { name: "Project", exact: true }).click();
  const panel = page.locator("#desk-canvas-project");
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

  await card.getByRole("button", { name: "Open in canvas", exact: true }).click();
  const canvas = page.locator("#desk-canvas-artifact");
  await expect(canvas).toBeVisible();
  await canvas.getByRole("button", { name: "Preview", exact: true }).click();
  await expect(canvas.locator(".canvas-artifact-preview")).toContainText(
    "Ready for review",
  );

  const download = page.waitForEvent("download");
  await canvas.getByRole("button", { name: "Download", exact: true }).click();
  const artifactDownload = await download;
  expect(artifactDownload.suggestedFilename()).toBe("release.md");

  await canvas.getByRole("button", { name: "Copy reference", exact: true }).click();
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

  await expect(page.getByRole("option", { name: /Release review/ })).toBeVisible();
  await page.getByRole("option", { name: /Release review/ }).click();
  await expect(page.locator("#desk-session-title")).toHaveText("Release review");
  await page.getByRole("button", { name: "Session and files", exact: true }).click();
  await page.getByRole("tab", { name: "Diagnostics", exact: true }).click();
  for (const [summary, button, result] of [
    ["Usage", /3 requests · 120 in · 45 out · 10 reserved/],
    ["Permissions", /Sandbox: workspace-write/],
    ["Working set", /Verify the Today experience/],
    ["Commands", /\/new · Start a conversation/],
  ]) {
    const panel = page.locator(".context-panels details").filter({ hasText: summary });
    await expect(panel.locator(".context-panel-result")).toContainText(button);
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
  // The navigate-away step closes the owner on pagehide; returning to Today
  // reattaches, finds the closed client, and silently opens fresh (#454).
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
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
      second.on("dialog", (dialog) => dialog.accept());
      const openBodies = [];
      const commandBodies = [];
      const eventRequests = [];
      second.on("request", (request) => {
        const url = new URL(request.url());
        if (url.pathname === "/api/v1/desk/chat/open" && request.method() === "POST") {
          openBodies.push(JSON.parse(request.postData() || "{}"));
        }
        if (url.pathname === "/api/v1/desk/chat/command" && request.method() === "POST") {
          commandBodies.push(JSON.parse(request.postData() || "{}"));
        }
        if (url.pathname === "/api/v1/desk/events") {
          eventRequests.push(request);
        }
      });
      await second.goto(deskURL("today"));

      // Bounded retries fail, then inline recovery appears; the fatal
      // out-of-date treatment is reserved for genuinely incompatible state.
      await expect(second.locator("#desk-stale-status")).toBeVisible({
        timeout: 15_000,
      });
      await expect(second.locator("#desk-phase")).toBeHidden();
      await expect(second.locator("#desk-stale-label")).toHaveText(
        "This conversation is open in another window.",
      );
      await expect(second.locator("#desk-transcript")).toHaveText(
        "This conversation is open in another window.",
      );
      await expect(second.locator("#desk-composer")).toBeHidden();
      await expect(second.locator("#desk-canvas-drawer")).toBeHidden();
      await expect(second.getByRole("button", { name: "Start new", exact: true })).toBeVisible();
      await expect(second.getByRole("button", { name: "Recent conversations", exact: true })).toBeVisible();
      await expect(second.getByRole("button", { name: "Refresh", exact: true })).toBeVisible();
      await second.getByRole("button", { name: "Recent conversations", exact: true }).click();
      await expect(second.locator("#desk-sessions")).toBeVisible();
      await expect(
        second.locator(".session-choice").filter({ hasText: "Release review" }),
      ).toHaveCount(1);
      await expect(second.locator(".session-menu-trigger")).toHaveCount(0);

      // Inline recovery opens a fresh session and returns the composer to a
      // usable state.
      await second.getByRole("button", { name: "Start new", exact: true }).click();
      await expect(second.locator("#desk-phase")).toHaveText("Ready");
      const recoveryOpen = openBodies.at(-1);
      expect(recoveryOpen).toMatchObject({
        continue: false,
        session_id: "",
        temporary: true,
      });
      expect(recoveryOpen.reattach_client_id).toBeUndefined();
      const newCommands = commandBodies.filter(({ command }) => command.name === "new");
      expect(newCommands.map(({ command }) => command)).toEqual([
        { name: "new", args: "" },
        { name: "new", args: "confirm" },
      ]);
      expect(newCommands[0].client_id).toBe(newCommands[1].client_id);
      await expect.poll(() => eventRequests.length).toBe(1);
      expect(eventRequests).toHaveLength(1);
      const owner = await second.evaluate(() =>
        JSON.parse(sessionStorage.getItem("waffle.desk.today.owner.v1")),
      );
      expect(owner.session_id).toBe("session-fresh");
      await expect(second.locator("#desk-sessions")).toBeVisible();
      await expect(second.locator("#desk-session-refresh")).toBeHidden();
      await expect(
        second.locator(".session-choice").filter({ hasText: "Release review" }),
      ).toHaveCount(1);
      await expect(
        second.locator(".session-choice").filter({ hasText: "Fresh conversation" }),
      ).toHaveCount(1);
      expect(commandBodies.findLast(({ command }) => command.name === "sessions")).toMatchObject({
        client_id: owner.client_id,
        command: { name: "sessions", args: "" },
      });
      await page.request.post(`${baseURL}/api/v1/desk/test/lock-latest?on=0`);
      await second.reload();
      await expect(second.locator("#desk-phase")).toHaveText("Ready");
      await expect(second.locator("#desk-sessions")).toBeVisible();
      await expect(second.locator("#desk-session-refresh")).toBeHidden();
      await expect(
        second.locator(".session-choice").filter({ hasText: "Fresh conversation" }),
      ).toHaveCount(1);
      await expect(
        second.locator(".session-choice").filter({ hasText: "Temporary conversation" }),
      ).toHaveCount(0);
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
  // The route abort below deliberately refuses the stream connection.
  allowDiagnostics("ERR_CONNECTION_REFUSED");

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
  // The reload races the keepalive close fired on pagehide; the reattach
  // finds the retiring client and silently opens fresh.
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
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
  test.skip(
    ["desktop", "mobile", "narrow"].includes(test.info().project.name)
      ? false
      : "Run the workspace lifecycle on desktop and mobile widths.",
  );
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
  // Metadata is ordered and truthful: profile is primary, opaque IDs stay
  // secondary and copyable.
  await expect(clean.locator(".waffle-fragment-facts")).toContainText("Profile");
  await expect(clean.locator(".waffle-fragment-facts")).toContainText("reviewer");
  await expect(clean.locator(".waffle-fragment-facts")).not.toContainText("session-primary");
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
  await page.getByRole("button", { name: "Session and files", exact: true }).click();
  await expect(page.locator("#desk-canvas-drawer")).toBeVisible();
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
    allowExpectedResponse(404, "/api/v1/desk/memory/attach");
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
  test.skip(
    ["desktop", "mobile", "narrow"].includes(test.info().project.name)
      ? false
      : "Run the memory lifecycle on desktop and mobile widths.",
  );
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
  // Each destination hop closes the chat owner on pagehide; the next Today
  // visit reattaches, finds the closed client, and silently opens fresh.
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
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
  test.skip(
    ["desktop", "mobile", "narrow"].includes(test.info().project.name)
      ? false
      : "Run the staged install flow on desktop and mobile widths.",
  );
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

test("profile editing is structured, distinct, and reviewable", async ({ page }) => {
  await page.goto(deskURL("capabilities"));
  await openCapabilityTab(page, "Profiles");
  const card = page.locator(".profile-card").filter({ hasText: "reviewer" });
  await expect(card).toBeVisible();
  // Actions are visually distinct: Edit is primary, Copy secondary, Delete
  // destructive and separated.
  const edit = card.getByRole("button", { name: "Edit", exact: true });
  await expect(edit).toHaveAttribute("data-action", "edit");
  await expect(edit).toBeVisible();
  await expect(card.getByRole("button", { name: "Copy", exact: true })).toHaveAttribute("data-action", "copy");
  await expect(card.getByRole("button", { name: "Delete", exact: true })).toHaveAttribute("data-action", "delete");
  await expect(card.getByRole("button", { name: "Copy", exact: true })).toBeVisible();
  await expect(card.getByRole("button", { name: "Delete", exact: true })).toBeVisible();

  // The editor groups fields into understandable sections.
  await edit.click();
  const form = page.locator("#profile-form");
  await expect(form).toBeVisible();
  await expect(form.locator("fieldset legend")).toContainText([
    "Identity & model",
    "Prompt",
    "Tools & policy",
    "Resource limits",
  ]);
  // Structured one-per-line tool controls replace comma-separated fields.
  await expect(form.getByLabel("Allowed tools (one per line)")).toHaveValue("read");
  await expect(form.getByLabel("Denied tools (one per line)")).toHaveValue("bash");
  await expect(form.getByLabel("Denied command prefixes (one per line)")).toHaveValue("git push");

  // Review identifies the direction of policy changes.
  await form.getByRole("button", { name: "Review change", exact: true }).click();
  const review = page.locator("#profile-review-dialog");
  await expect(review).toBeVisible();
  await expect(review).toContainText("Effective policy before");
  await expect(review).toContainText("Effective policy after");
  await review.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(review).toBeHidden();

  // Delete is an explicit, confirmable destructive path.
  await card.getByRole("button", { name: "Delete", exact: true }).click();
  await expect(review).toBeVisible();
  await expect(review.getByRole("button", { name: "Delete profile", exact: true })).toBeDisabled();
  await review.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(review).toBeHidden();
});

test("Capabilities leads with scannable summaries and compact tabs", async ({ page }) => {
  await page.goto(deskURL("capabilities"));
  // Models summary states the Waffle-wide roles above the inventory.
  await expect(page.locator("#capability-models-summary")).toContainText("Default:", { timeout: 10_000 });
  await expect(page.locator("#capability-models-summary")).toContainText("aliases");
  // Skills summary counts active skills.
  await openCapabilityTab(page, "Skills");
  await expect(page.locator("#capability-skills-summary")).toContainText("skills", { timeout: 10_000 });
  // Connections summary counts health.
  await openCapabilityTab(page, "Tools & connections");
  await expect(page.locator("#capability-connections-summary")).toContainText("connections", { timeout: 10_000 });
  await expect(page.locator("#capability-connections-summary")).toContainText("need attention");
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
  const before = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    visualViewportWidth: window.visualViewport?.width ?? 0,
  }));
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: 368,
    height: 250,
    deviceScaleFactor: 1,
    mobile: false,
    screenWidth: 735,
    screenHeight: 500,
  });
  const after = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    visualViewportWidth: window.visualViewport?.width ?? 0,
  }));
  expect(after.innerWidth).toBeLessThan(before.innerWidth);
  expect(after.visualViewportWidth).toBe(after.innerWidth);

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

test("Task 4: Today Hearth auto-loads grouped history while other sections keep the shared rail", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the Task 4 history rail proof once.");
  await page.setViewportSize({ width: 1470, height: 920 });
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await expect(page.locator("#desk-session-rail")).toBeVisible();
  await expect(page.locator("#desk-new")).toBeVisible();
  await expect(page.locator("#desk-session-drawer-close")).toBeHidden();
  for (const label of ["Pinned", "Today", "Yesterday", "Previous 7 days", "Older"]) {
    await expect(page.locator("#desk-session-options")).toContainText(label);
  }
  const historyScroll = await page.locator("#desk-session-options").evaluate((element) => ({
    scrollHeight: element.scrollHeight,
    clientHeight: element.clientHeight,
  }));
  expect(historyScroll.scrollHeight).toBeGreaterThan(historyScroll.clientHeight);
  await page.locator("#desk-session-options").evaluate((element) => {
    element.scrollTop = element.scrollHeight;
  });
  await expect(page.locator("#desk-session-options")).toContainText("Older archive");
  const newConversation = await page.locator("#desk-new").evaluate((element) => {
    const button = element.getBoundingClientRect();
    const drawer = element.parentElement.getBoundingClientRect();
    return {
      button,
      drawer,
      background: getComputedStyle(element).backgroundColor,
    };
  });
  expect(newConversation.button.width).toBeCloseTo(newConversation.drawer.width, 0);
  expect(newConversation.background).toBe("rgb(221, 113, 40)");
  await page.getByLabel("Theme").selectOption("dark");
  const darkNewConversation = await page.locator("#desk-new").evaluate((element) => {
    const style = getComputedStyle(element);
    return { text: element.textContent.trim(), color: style.color, background: style.backgroundColor };
  });
  expect(darkNewConversation.text).toBe("New conversation");
  expect(
    contrastRatio(darkNewConversation.color, darkNewConversation.background),
    "Evening New conversation label contrast",
  ).toBeGreaterThanOrEqual(4.5);
  await expect(page.locator(".task-context")).toHaveCount(0);
  await page.goto(deskURL("tasks"));
  await expect(page.locator("#desk-session-rail")).toHaveCount(0);
  await expect(page.locator(".section-links")).toBeVisible();
});

test("Task 4: Split Kiln crosses the exact breakpoint with a true split and viewport overlay", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Run the Task 4 canvas proof once.");
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  await page.setViewportSize({ width: 1470, height: 920 });
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  const toggle = page.getByRole("button", { name: /Session and files/i });
  await toggle.click();
  await expect(page.locator("#desk-canvas-drawer")).toBeVisible();
  await expect(page.locator("#desk-canvas-tab-artifact")).toBeHidden();
  await expect(page.locator(".desk-shell")).toHaveAttribute("data-canvas", "open");
  const split = await page.evaluate(() => {
    const columns = document.querySelector(".today-columns").getBoundingClientRect();
    const conversation = document.querySelector(".conversation").getBoundingClientRect();
    const canvas = document.querySelector("#desk-canvas-drawer").getBoundingClientRect();
    return { columns, conversation, canvas, viewport: window.innerWidth, role: document.querySelector("#desk-canvas-drawer").getAttribute("role") };
  });
  expect(split.canvas.left).toBeGreaterThan(split.conversation.left);
  expect(split.canvas.width / split.columns.width).toBeGreaterThan(0.4);
  expect(split.canvas.width / split.columns.width).toBeLessThan(0.5);
  expect(split.role).toBe("complementary");

  await page.setViewportSize({ width: 1100, height: 920 });
  await expect(page.locator("#desk-canvas-drawer")).toHaveAttribute("role", "complementary");
  const exactSplit = await page.locator("#desk-canvas-drawer").evaluate((element) => {
    const columns = document.querySelector(".today-columns").getBoundingClientRect();
    const canvas = element.getBoundingClientRect();
    return {
      ratio: canvas.width / columns.width,
      role: element.getAttribute("role"),
      modal: element.getAttribute("aria-modal"),
      position: getComputedStyle(element).position,
    };
  });
  expect(exactSplit.ratio).toBeGreaterThan(0.4);
  expect(exactSplit.ratio).toBeLessThan(0.5);
  expect(exactSplit.role).toBe("complementary");
  expect(exactSplit.modal).toBeNull();
  expect(exactSplit.position).not.toBe("fixed");

  await page.setViewportSize({ width: 1099, height: 920 });
  await expect(page.locator("#desk-canvas-drawer")).toHaveAttribute("role", "dialog");
  const overlay = await page.locator("#desk-canvas-drawer").evaluate((element) => {
    const canvas = element.getBoundingClientRect();
    const rail = document.querySelector(".desk-navigation").getBoundingClientRect();
    return {
      canvas,
      rail,
      role: element.getAttribute("role"),
      modal: element.getAttribute("aria-modal"),
      position: getComputedStyle(element).position,
      viewport: { width: window.innerWidth, height: window.innerHeight },
    };
  });
  expect(overlay.role).toBe("dialog");
  expect(overlay.modal).toBe("true");
  expect(overlay.position).toBe("fixed");
  expect(overlay.canvas.left).toBeGreaterThanOrEqual(overlay.rail.right - 1);
  expect(overlay.canvas.top).toBeGreaterThan(0);
  expect(overlay.canvas.bottom).toBeLessThanOrEqual(overlay.viewport.height);

  await page.getByRole("tab", { name: "Session" }).click();
  await page.locator("#desk-posture-open").focus();
  await page.keyboard.press("Tab");
  await expect(page.locator("#desk-canvas-close")).toBeFocused();

  await page.getByRole("tab", { name: "Diagnostics" }).click();
  await page.locator("#desk-canvas-close").focus();
  await page.keyboard.press("Shift+Tab");
  await expect(page.getByRole("tab", { name: "Project" })).toBeFocused();

  for (const viewport of [{ width: 768, height: 1000 }, { width: 375, height: 812 }]) {
    await page.setViewportSize(viewport);
    const compact = await page.locator("#desk-canvas-drawer").evaluate((element) => {
      const canvas = element.getBoundingClientRect();
      const navigation = document.querySelector(".desk-navigation").getBoundingClientRect();
      return {
        canvas,
        navigation,
        role: element.getAttribute("role"),
        modal: element.getAttribute("aria-modal"),
        position: getComputedStyle(element).position,
        viewport: { width: window.innerWidth, height: window.innerHeight },
      };
    });
    expect(compact.role, `${viewport.width}px canvas role`).toBe("dialog");
    expect(compact.modal, `${viewport.width}px canvas modal`).toBe("true");
    expect(compact.position, `${viewport.width}px canvas positioning`).toBe("fixed");
    expect(compact.canvas.left, `${viewport.width}px canvas left`).toBeGreaterThanOrEqual(0);
    expect(compact.canvas.right, `${viewport.width}px canvas right`).toBeLessThanOrEqual(compact.viewport.width);
    expect(compact.canvas.bottom, `${viewport.width}px canvas clears bottom nav`).toBeLessThanOrEqual(compact.navigation.top + 1);
    await expect(page.locator("#desk-canvas-drawer")).toBeVisible();
    await expect(page.locator("#desk-canvas-close")).toBeVisible();
  }

  const canvas = page.locator("#desk-canvas-drawer");
  await expect(canvas.getByRole("button", { name: /Close/i })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(canvas).toBeHidden();
  await expect(page.getByRole("button", { name: /Session and files/i })).toBeFocused();
  await expectNoHorizontalOverflow(page);
});

test("Task 4: palette Find a conversation reveals the compact drawer before focusing its filter", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(deskURL("today"));
  await expect(page.locator("#desk-phase")).toHaveText("Ready");
  await page.locator("#desk-context-toggle").click();
  await expect(page.locator("#desk-canvas-drawer")).toBeVisible();
  await page.locator("#palette-open").click();
  await page.locator("#palette-search").fill("Find a conversation");
  const command = page.getByRole("option", { name: /Find a conversation/i });
  await expect(command).toBeVisible();
  await command.click();
  await expect(page.locator("#command-palette")).toBeHidden();
  await expect(page.locator("#desk-canvas-drawer")).toBeHidden();
  await expect(page.locator("#desk-sessions")).toBeVisible();
  const filter = page.locator("#desk-session-filter");
  await expect(filter).toBeFocused();
  await expect(filter).toBeInViewport();
  expect(await filter.evaluate((element) => {
    const bounds = element.getBoundingClientRect();
    const topmost = document.elementFromPoint(
      bounds.left + bounds.width / 2,
      bounds.top + bounds.height / 2,
    );
    return topmost === element || element.contains(topmost);
  })).toBe(true);
});

test("Task 4: mobile Conversations drawer clears the composer, bottom tabs, and Escape focus", async ({ page }) => {
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  for (const width of [768, 375]) {
    await page.setViewportSize({ width, height: 812 });
    await page.goto(deskURL("today"));
    await expect(page.locator("#desk-phase")).toHaveText("Ready");
    const opener = page.getByRole("button", { name: "Conversations", exact: true });
    await opener.click();
    const drawer = page.locator("#desk-sessions");
    await expect(drawer).toBeVisible();
    await expect(page.locator("#desk-session-drawer-close")).toBeVisible();
    const geometry = await page.evaluate(() => {
      const rect = (selector) => {
        const bounds = document.querySelector(selector).getBoundingClientRect();
        return { top: bounds.top, bottom: bounds.bottom, left: bounds.left, right: bounds.right };
      };
      return {
        drawer: rect("#desk-sessions"),
        composer: rect("#desk-composer"),
        send: rect("#desk-send"),
        navigation: rect(".desk-navigation"),
      };
    });
    expect(geometry.drawer.bottom, `${width}px drawer above composer`).toBeLessThanOrEqual(geometry.composer.top + 1);
    expect(geometry.drawer.bottom, `${width}px drawer above bottom navigation`).toBeLessThanOrEqual(geometry.navigation.top + 1);
    expect(geometry.composer.bottom, `${width}px composer above bottom navigation`).toBeLessThanOrEqual(geometry.navigation.top + 1);
    expect(geometry.send.bottom, `${width}px send above bottom navigation`).toBeLessThanOrEqual(geometry.navigation.top + 1);
    await page.keyboard.press("Escape");
    await expect(drawer).toBeHidden();
    await expect(opener).toBeFocused();
  }
});

test("Task 4: Hearth and Split Kiln evidence covers Hearth and Evening closed, history-open, and canvas states", async ({ page }) => {
  test.skip(test.info().project.name !== "desktop", "Capture the reviewed Task 4 evidence once.");
  allowExpectedResponse(404, "/api/v1/desk/chat/open");
  for (const theme of ["light", "dark"]) {
    for (const width of [1470, 375]) {
      await page.setViewportSize({ width, height: 920 });
      await page.goto(deskURL("today"));
      await page.getByLabel("Theme").selectOption(theme);
      await expect(page.locator("#desk-phase")).toHaveText("Ready");
      const name = (state) => path.join(
        "test-results",
        `task4-${theme}-${width}-${state}.png`,
      );
      await page.screenshot({ path: name("hearth-closed"), fullPage: false });
      if (width <= 768) {
        await page.getByRole("button", { name: "Conversations", exact: true }).click();
        await expect(page.locator("#desk-sessions")).toBeVisible();
      }
      await page.screenshot({ path: name("hearth-history-open"), fullPage: false });
      if (width <= 768) {
        await page.keyboard.press("Escape");
      }
      await page.getByRole("button", { name: /Session and files/i }).click();
      await expect(page.locator("#desk-canvas-drawer")).toBeVisible();
      await page.screenshot({ path: name("split-kiln"), fullPage: false });
    }
  }
});
