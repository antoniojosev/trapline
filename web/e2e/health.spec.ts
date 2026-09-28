// The second half of the panel gate: what survived the restart.
//
// `smoke.spec.ts` reported twenty sessions, one of them a crash, and then
// `scripts/ui-smoke.sh` stopped the server with SIGTERM and started it again.
// That is the flush: counters live in a bounded window in memory and are
// written on a timer or on a clean shutdown (ADR 008). So this file is not a
// continuation of the story — it is the part of the story that can only be
// told by a process that has been restarted.
//
// It runs on its own, from a clean browser with no cookie, and skips itself
// when it is invoked outside that arrangement. A file that quietly checked a
// number the server had not written yet would fail for a reason nobody could
// read, and a file that waited sixty seconds for the timer instead would turn
// a pull-request gate into a coffee break.

import { expect, test, type Page } from "@playwright/test";
import { ADMIN, PASSWORD } from "./credentials";

const RELEASE = "ui-smoke@1.1.0";
// The release every event of the first half carried, and the one nobody
// reported a single session for. It is here to be checked for silence.
const QUIET_RELEASE = "ui-smoke@1.0.0";

test.describe.configure({ mode: "serial" });

test.skip(
  process.env["TRAPLINE_AFTER_RESTART"] !== "1",
  "needs a server that has been restarted since the sessions were reported; run scripts/ui-smoke.sh",
);

/**
 * signIn opens a session and answers with the project this run created.
 *
 * The id comes from the API rather than from a link on the projects screen:
 * this file starts from a clean browser every test, and threading a click path
 * through three screens to reach the one under test is three more ways for a
 * failure here to be about something else.
 *
 * That it works at all is the first thing being checked. This browser has
 * never met this process — the one that issued the cookie is gone — and a
 * session that lived in the server's memory would have died with it.
 */
async function signIn(page: Page): Promise<number> {
  await page.goto("/");
  await page.getByLabel("Username").fill(ADMIN);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();

  const response = await page.request.get("/api/v1/projects");
  expect(response.status()).toBe(200);
  const projects = (await response.json()) as { id: number }[];
  const first = projects[0];
  if (!first) throw new Error("the restarted server has no projects");
  return first.id;
}

test("the session outlives the process that issued it", async ({ page }) => {
  await signIn(page);
});

test("the release list answers which release is the bad one", async ({
  page,
}) => {
  const id = await signIn(page);
  await page.goto(`/projects/${id}/releases`);

  await expect(page.getByRole("heading", { name: /Releases/ })).toBeVisible();

  // Seventeen exited, two errored and still exited, one crashed: twenty
  // sessions, 95% crash-free. One request for the whole page and not one per
  // row — the project-wide endpoint returns every release that reported.
  const reported = page.getByRole("listitem").filter({ hasText: RELEASE });
  await expect(reported).toContainText("95% crash-free");

  // And a release nobody reported sessions for says nothing at all, rather
  // than a zero that reads as "everything crashed" or a dash that reads as
  // "we checked and it is fine".
  const quiet = page.getByRole("listitem").filter({ hasText: QUIET_RELEASE });
  await expect(quiet).not.toContainText("crash-free");
});

test("the release detail carries the number and the caveat that comes with it", async ({
  page,
}) => {
  const id = await signIn(page);
  await page.goto(`/projects/${id}/releases/${encodeURIComponent(RELEASE)}`);

  await expect(
    page.getByRole("heading", { name: "Release health" }),
  ).toBeVisible();

  // The four disjoint counters. A crashed session is not also an errored one,
  // which is what makes them add up to the total rather than over it.
  await expect(page.getByText("of sessions ended cleanly")).toBeVisible();
  await expect(page.getByText("started in this range")).toBeVisible();
  await expect(page.getByText("ended in a crash")).toBeVisible();
  await expect(page.getByText("saw an error but survived")).toBeVisible();

  // The caveat is on the screen, beside the number, and not in a document
  // nobody opens: the newest hour can move after a restart, and a reader who
  // discovers that during an incident stops trusting the whole panel (ADR 008).
  await expect(
    page.getByText(/figures for the most recent hour may change after a restart/),
  ).toBeVisible();
});
