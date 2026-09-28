import { defineConfig, devices } from "@playwright/test";

// The server is not started from here. It is a real build of the binary, boot
// ed by scripts/ui-smoke.sh on the host, and this suite runs inside a
// container that reaches it through host.docker.internal — so what the browser
// talks to is the same embedded panel a user gets, served by the same process
// that answers the API, rather than a Vite dev server with a proxy in front.
const baseURL = process.env["TRAPLINE_BASE_URL"];
if (!baseURL) {
  throw new Error("TRAPLINE_BASE_URL is required; run this through scripts/ui-smoke.sh");
}

export default defineConfig({
  testDir: ".",
  // Outside the repo's source tree and gitignored: a failing run leaves
  // screenshots and a trace behind, and neither is something to commit.
  outputDir: "../../artifacts/ui",
  fullyParallel: false,
  workers: 1,
  // No retries. This suite drives one server through one accumulating story —
  // a project is created, events are ingested, an issue is resolved — so a
  // retry would replay half of it against state the first attempt already
  // changed and report a failure nobody can read.
  retries: 0,
  forbidOnly: true,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: [["list"]],
  use: {
    baseURL,
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
    video: "off",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
