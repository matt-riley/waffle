/**
 * Capture Waffle Desk screenshots for the documentation site.
 *
 * Screenshots in docs go stale silently: the UI moves, the picture does not,
 * and nothing fails. This regenerates them from the same deterministic Go
 * fixture the browser gate drives (fixtures/fake-server.go), so a capture is
 * reproducible and never contains real data — the fixture listens on an
 * ephemeral loopback port and contacts no provider, Git host, or container.
 *
 * Run from the repository root:
 *
 *   mise run docs-screenshots
 *
 * Uses system Chrome by default, matching playwright.config.mjs. Set
 * PLAYWRIGHT_CHROMIUM_PATH to point at another Chromium build when system
 * Chrome is unavailable.
 *
 * The fixture bootstrap below is intentionally a small standalone copy rather
 * than an import from tests/desk.spec.mjs: importing that module would execute
 * its test registrations. Both drive the same fixture binary, which is the
 * part that actually has to agree.
 */
import { chromium } from "@playwright/test";
import { spawn } from "node:child_process";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";

const packageDir = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(packageDir, "../..");
const outputDir = path.join(repositoryRoot, "website/src/assets/screenshots");

/**
 * Sections worth a picture in the docs, and the file each becomes.
 *
 * Deliberately not "today": the fixture models a half-configured install so it
 * can exercise the setup checklist, so Today always renders a "Waffle is not
 * set up yet" banner over a still-loading conversation. Accurate for the
 * fixture, misleading in docs — and dressing it up would mean screenshotting
 * something that is not the real thing.
 */
const SHOTS = [
	{ section: "tasks", file: "desk-tasks.png", waitFor: ".tasks, main" },
	{ section: "capabilities", file: "desk-capabilities.png", waitFor: "main" },
];

const VIEWPORT = { width: 1280, height: 860 };

function stopFixture(child) {
	if (!child?.pid) return;
	try {
		process.kill(-child.pid, "SIGTERM");
	} catch {
		child.kill("SIGTERM");
	}
}

async function startFixture() {
	const child = spawn("go", ["run", "./tools/dashboard-tests/fixtures/fake-server.go"], {
		cwd: repositoryRoot,
		env: {
			...process.env,
			GOCACHE: process.env.GOCACHE || path.join(os.tmpdir(), "waffle-dashboard-go-build"),
		},
		detached: true,
		stdio: ["ignore", "pipe", "pipe"],
	});

	let errors = "";
	child.stderr.setEncoding("utf8");
	child.stderr.on("data", (chunk) => {
		errors += chunk;
	});

	const output = readline.createInterface({ input: child.stdout });

	// Every failure path from here has to stop the child: it is detached, so an
	// early return would leave a stray `go run` holding its port until the shell
	// that started it goes away.
	try {
		const url = await new Promise((resolve, reject) => {
			const timeout = setTimeout(() => {
				reject(new Error(`dashboard fixture timed out\n${errors}`));
			}, 120_000);
			const settle = (fn) => (value) => {
				clearTimeout(timeout);
				output.off("line", onLine);
				child.off("exit", onExit);
				fn(value);
			};
			const onLine = (line) => settle(resolve)(line);
			const onExit = (code) =>
				settle(reject)(new Error(`dashboard fixture exited (code ${code})\n${errors}`));

			output.on("line", onLine);
			child.on("exit", onExit);
		});

		if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(url)) {
			throw new Error(`unexpected dashboard fixture URL: ${url}\n${errors}`);
		}

		return { child, url };
	} catch (error) {
		stopFixture(child);
		throw error;
	} finally {
		output.close();
	}
}

async function main() {
	await mkdir(outputDir, { recursive: true });

	const { child, url } = await startFixture();
	let browser;

	try {
		const executablePath = process.env.PLAYWRIGHT_CHROMIUM_PATH;
		browser = await chromium.launch(
			executablePath ? { executablePath } : { channel: "chrome" },
		);
		const context = await browser.newContext({
			viewport: VIEWPORT,
			colorScheme: "light",
			// Docs screenshots must not catch a half-finished transition.
			reducedMotion: "reduce",
			deviceScaleFactor: 2,
		});
		const page = await context.newPage();

		for (const { section, file, waitFor } of SHOTS) {
			const response = await page.goto(`${url}/desk/?section=${encodeURIComponent(section)}`);
			if (response?.status() !== 200) {
				throw new Error(`section ${section} returned ${response?.status()}`);
			}
			if (waitFor) {
				await page.locator(waitFor).first().waitFor({ state: "visible" });
			}
			await page.waitForLoadState("networkidle");

			const target = path.join(outputDir, file);
			await page.screenshot({ path: target });
			console.log(`captured ${section} -> ${path.relative(repositoryRoot, target)}`);
		}
	} finally {
		if (browser) await browser.close();
		stopFixture(child);
	}
}

await main();
