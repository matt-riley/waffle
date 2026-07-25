import { spawn } from "node:child_process";
import readline from "node:readline";
import { fileURLToPath } from "node:url";

const repositoryRoot = fileURLToPath(new URL("../../..", import.meta.url));

export async function startFixture() {
  const child = spawn("go", ["run", "./tools/dashboard-tests/fixtures/fake-server.go"], {
    cwd: repositoryRoot,
    env: { ...process.env, GOCACHE: process.env.GOCACHE || "/tmp/waffle-dashboard-go-build" },
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
  });
  const output = readline.createInterface({ input: child.stdout });
  const url = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error("dashboard fixture timed out")), 120_000);
    const onLine = (line) => {
      clearTimeout(timeout);
      output.off("line", onLine);
      resolve(line);
    };
    output.on("line", onLine);
    child.once("error", reject);
    child.once("exit", (code) => reject(new Error(`dashboard fixture exited before readiness: ${code}`)));
  });
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(url)) {
    throw new Error(`unexpected dashboard fixture URL: ${url}`);
  }
  return { child, url };
}

export async function getFragment(url, path) {
  const response = await fetch(`${url}${path}`, {
    headers: { Accept: "text/html", "HX-Request": "true", Origin: url },
  });
  return { response, body: await response.text() };
}

export async function stopFixture(child) {
  if (!child || child.exitCode !== null) return;
  try {
    process.kill(-child.pid, "SIGTERM");
  } catch {
    child.kill("SIGTERM");
  }
  await new Promise((resolve) => child.once("exit", resolve));
}

