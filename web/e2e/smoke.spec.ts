// The panel, in a real browser, doing the one job it exists for.
//
// Every screen here was already covered by Go tests against the API and by
// TypeScript against itself, and neither of those can tell you that the page
// renders, that the button is reachable, or that the colours are the ones the
// reader asked their operating system for. Until this file existed the panel
// had never been opened by anything but a person, occasionally.
//
// The story is one story on purpose — set up, sign in, create a project,
// ingest, group, triage, regress, switch a category off and back on — because
// that is the order the pieces are used in, and a bug in the seam between two
// of them is invisible to a test that only ever visits one. That also means
// one browser context for the whole file: the session is a cookie, and a test
// that starts from a clean context starts logged out.

import { execFileSync } from "node:child_process";
import {
  expect,
  test,
  type BrowserContext,
  type Page,
} from "@playwright/test";

import { ADMIN, PASSWORD, PROJECT } from "./credentials";

test.describe.configure({ mode: "serial" });

let context: BrowserContext;
let page: Page;
// Read off the screen in the third test, the way a user reads it.
let dsn = "";

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext();
  page = await context.newPage();
});

test.afterAll(async () => {
  await context.close();
});

/**
 * ingest posts one envelope with curl, exactly as the README tells a user to
 * check their DSN.
 *
 * curl rather than fetch because the DSN is a contract with the outside world,
 * and this is the cheapest client that has no idea what this project believes
 * the protocol to be — the same reasoning that makes the compatibility matrix
 * use the official SDKs rather than a hand-written envelope (ADR 002).
 */
function ingest(
  eventID: string,
  options: { release?: string; type?: string } = {},
): number {
  const match = /^(https?):\/\/([^@]+)@([^/]+)\/(\d+)$/.exec(dsn);
  if (!match) throw new Error(`the panel showed a DSN we cannot parse: ${dsn}`);
  const [, scheme, key, host, projectID] = match;

  // Identical apart from the event id, so this exercises grouping rather than
  // counting: four occurrences of one bug must become one issue seen 4 times.
  // The release and the exception type are the two things a caller may vary,
  // because they are what the release story below is about — a different type
  // is a different issue, and a different release is what decides whether an
  // event is a regression or an expected straggler.
  const event = JSON.stringify({
    event_id: eventID,
    timestamp: new Date().toISOString(),
    platform: "python",
    level: "error",
    release: options.release ?? "ui-smoke@1.0.0",
    environment: "production",
    exception: {
      values: [
        {
          type: options.type ?? "ValueError",
          value: "invalid amount 4821",
          stacktrace: {
            frames: [
              {
                abs_path: "/srv/app/views.py",
                function: "checkout",
                lineno: 42,
                in_app: true,
              },
            ],
          },
        },
      ],
    },
    tags: { server: "web-01" },
  });

  const envelope =
    `${JSON.stringify({ event_id: eventID })}\n` +
    `${JSON.stringify({ type: "event", length: Buffer.byteLength(event) })}\n` +
    `${event}\n`;

  const status = execFileSync(
    "curl",
    [
      "-sS",
      "-o",
      "/dev/null",
      "-w",
      "%{http_code}",
      "-X",
      "POST",
      `${scheme}://${host}/api/${projectID}/envelope/`,
      "-H",
      `X-Sentry-Auth: Sentry sentry_version=7, sentry_client=ui-smoke/1.0, sentry_key=${key}`,
      "--data-binary",
      "@-",
    ],
    { input: envelope, encoding: "utf8" },
  );
  return Number(status.trim());
}

/**
 * postItem sends one envelope carrying one item of any type.
 *
 * `ingest` above is the error-shaped special case, kept as it was because it
 * is what the README tells a user to run. This is the same envelope with the
 * item type and body handed in, which is what the tracing and session items
 * need — and it goes through curl for the same reason: the protocol is a
 * contract with the outside world, and the cheapest client with no idea what
 * this project believes about it is the one worth checking against (ADR 002).
 */
function postItem(type: string, payload: string): number {
  const match = /^(https?):\/\/([^@]+)@([^/]+)\/(\d+)$/.exec(dsn);
  if (!match) throw new Error(`the panel showed a DSN we cannot parse: ${dsn}`);
  const [, scheme, key, host, id] = match;

  const envelope =
    `{}\n` +
    `${JSON.stringify({ type, length: Buffer.byteLength(payload) })}\n` +
    `${payload}\n`;

  const status = execFileSync(
    "curl",
    [
      "-sS",
      "-o",
      "/dev/null",
      "-w",
      "%{http_code}",
      "-X",
      "POST",
      `${scheme}://${host}/api/${id}/envelope/`,
      "-H",
      `X-Sentry-Auth: Sentry sentry_version=7, sentry_client=ui-smoke/1.0, sentry_key=${key}`,
      "--data-binary",
      "@-",
    ],
    { input: envelope, encoding: "utf8" },
  );
  return Number(status.trim());
}

// Ids that are deterministic rather than random: a failing run has to be
// readable, and "which of these thirty traces" is not a question anybody
// should have to answer from a random hex string.
let traceCounter = 0;
function hexID(width: number): string {
  traceCounter += 1;
  return traceCounter.toString(16).padStart(width, "0");
}

/**
 * sendTransaction posts one transaction with a child span.
 *
 * A child rather than a bare root because the screen under test is a
 * waterfall, and a waterfall of one bar proves only that the page rendered.
 * Both timestamps are sent: the server refuses a transaction with one end and
 * is right to, because a duration completed from the server's clock measures
 * the queue rather than the request (ADR 021).
 */
function sendTransaction(options: {
  name: string;
  durationMS: number;
  failed?: boolean;
}): number {
  const end = new Date();
  const start = new Date(end.getTime() - options.durationMS);
  const rootSpan = hexID(16);
  const payload = JSON.stringify({
    event_id: hexID(32),
    transaction: options.name,
    platform: "python",
    release: "ui-smoke@1.1.0",
    environment: "production",
    start_timestamp: start.toISOString(),
    timestamp: end.toISOString(),
    contexts: {
      trace: {
        trace_id: hexID(32),
        span_id: rootSpan,
        op: "http.server",
        status: options.failed ? "internal_error" : "ok",
      },
    },
    spans: [
      {
        span_id: hexID(16),
        parent_span_id: rootSpan,
        op: "db.sql",
        description: "SELECT 1",
        status: "ok",
        start_timestamp: new Date(start.getTime() + 5).toISOString(),
        timestamp: new Date(end.getTime() - 5).toISOString(),
      },
    ],
  });
  return postItem("transaction", payload);
}

function projectID(): string {
  const id = /\/(\d+)$/.exec(dsn)?.[1];
  if (!id) throw new Error(`no project id in the DSN: ${dsn}`);
  return id;
}

async function fillCredentials() {
  await page.getByLabel("Username").fill(ADMIN);
  await page.getByLabel("Password").fill(PASSWORD);
}

test("the first run creates an admin and closes registration behind it", async () => {
  await page.goto("/");

  await expect(
    page.getByRole("heading", { name: "Create your admin account" }),
  ).toBeVisible();
  await fillCredentials();
  await page.getByRole("button", { name: "Create account" }).click();

  // Setup signs the new admin straight in: making someone retype a password
  // they chose two seconds ago is friction with no security value.
  await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();
});

test("signing out and back in gets to the same place", async () => {
  await page.getByRole("button", { name: "Sign out" }).click();

  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  await fillCredentials();
  await page.getByRole("button", { name: "Sign in" }).click();

  await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();
});

test("a new project hands over a DSN that works", async () => {
  await page.getByLabel("New project").fill(PROJECT);
  await page.getByRole("button", { name: "Create", exact: true }).click();

  await expect(page.getByRole("heading", { name: /ui-smoke/ })).toBeVisible();

  // The DSN is the entire migration story, so it is read off the screen the
  // way a user reads it rather than fetched from the API behind the panel's
  // back. The copy button beside it needs a secure context and is silently
  // unavailable over plain HTTP, which is how most self-hosted installs run;
  // that it degrades to selectable text instead of throwing is exactly the
  // behaviour being relied on here.
  dsn = (await page.locator("code").first().innerText()).trim();
  expect(dsn).toMatch(/^https?:\/\/[^@]+@[^/]+\/\d+$/);
  await expect(page.getByRole("button", { name: "copy" })).toBeVisible();

  for (const id of ["a", "b", "c", "d"]) {
    expect(ingest(id.repeat(32))).toBe(200);
  }
});

test("four occurrences of one bug are one issue, seen four times", async () => {
  await page.getByRole("link", { name: "Issues" }).first().click();

  // The heading is fed by the listing itself rather than by a second request,
  // so it has to be right on the first paint of the rows underneath it.
  await expect(page.getByRole("heading", { name: /ui-smoke/ })).toBeVisible();

  const rows = page.getByRole("listitem");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("ValueError");
  await expect(rows.first()).toContainText("4×");
  // The counts label the filter buttons and cover the project, not the filter.
  await expect(
    page.getByRole("button", { name: /Unresolved\s*1/ }),
  ).toBeVisible();
});

test("the detail is what someone opens to fix the bug", async () => {
  await page.getByRole("link", { name: /ValueError/ }).click();

  await expect(page.getByRole("heading", { name: "ValueError" })).toBeVisible();
  await expect(page.getByText("invalid amount 4821").first()).toBeVisible();

  await expect(
    page.getByRole("heading", { name: /^Stacktrace/ }),
  ).toBeVisible();
  await expect(page.getByText("/srv/app/views.py").first()).toBeVisible();
  await expect(page.getByText("checkout").first()).toBeVisible();

  await expect(page.getByRole("heading", { name: /^Tags across/ })).toBeVisible();
  await expect(page.getByText("web-01").first()).toBeVisible();
  await expect(page.getByText("production").first()).toBeVisible();
});

test("the issue can be handed to an agent without leaving the page", async () => {
  // The fourth client's way in for somebody who is not running one: the whole
  // issue as markdown, on the clipboard, in one click. Without this the
  // agent-first story starts with "find an API token and write a curl command"
  // (ADR 006, ADR 022).
  //
  // It matters that this runs *here*: the browser reaches the panel through
  // `host.docker.internal`, which is not a secure context, so
  // `navigator.clipboard` does not exist — exactly as it does not exist for
  // anyone serving this over plain HTTP on a VPS. A button that only worked on
  // localhost would pass every developer's manual check and fail for the first
  // real user.
  const copy = page.getByRole("button", { name: "Copy for an agent" });
  await expect(copy).toBeVisible();
  await copy.click();
  await expect(
    page.getByRole("button", { name: "Copied" }),
  ).toBeVisible();
});

test("resolving it, and a new event bringing it back as a regression", async () => {
  // Exact, because there are now two: this one and "Resolve in next release",
  // which the release story below is about.
  await page.getByRole("button", { name: "Resolve", exact: true }).click();
  await expect(
    page.getByText("A new event reopens this as a regression."),
  ).toBeVisible();

  // Gone from the default view, which is the point of resolving something.
  // Exact, because the release links on this page are named after the project
  // too: `ui-smoke@1.0.0` contains `ui-smoke`.
  await page.getByRole("link", { name: PROJECT, exact: true }).click();
  await expect(page.getByText(/Nothing unresolved/)).toBeVisible();

  expect(ingest("e".repeat(32))).toBe(200);

  await page.reload();
  const rows = page.getByRole("listitem");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("ValueError");
  await expect(rows.first()).toContainText("5×");
});

test("switching a category off refuses it, and switching it back on accepts it", async () => {
  await page.getByRole("link", { name: "Settings" }).first().click();

  await expect(page.getByRole("heading", { name: "Categories" })).toBeVisible();
  // A project nobody has configured shows the profile it inherits, and says so.
  // Showing the same list without saying where it comes from is how a form
  // talks an operator into freezing it.
  await expect(page.getByText("(default)").first()).toBeVisible();
  await expect(
    page.getByText(/Inheriting the default profile: error/),
  ).toBeVisible();

  const errors = page.getByRole("checkbox", { name: "error" });
  await expect(errors).toBeChecked();
  await errors.uncheck();
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByRole("status")).toContainText("Saved");

  // 429 rather than 200-and-discard: the protocol's own backpressure is what
  // makes a switched-off subsystem cost nothing, because the SDK stops sending
  // instead of retrying (ADR 005).
  expect(ingest("f".repeat(32))).toBe(429);

  await errors.check();
  await page.getByRole("button", { name: "Save" }).click();
  await page.reload();
  await expect(page.getByRole("checkbox", { name: "error" })).toBeChecked();

  // And back on with the very next event, not once some cache expires.
  expect(ingest("0".repeat(32))).toBe(200);
});

test("editing one setting does not silently freeze the others", async () => {
  await page.goto(`/projects/${projectID()}/settings`);

  const limit = page.getByLabel("Rate limit per minute");
  await expect(limit).toHaveValue("");
  await expect(limit).toHaveAttribute("placeholder", /\d+/);

  await limit.fill("600");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByRole("status")).toContainText("Saved");

  await page.reload();
  await expect(page.getByLabel("Rate limit per minute")).toHaveValue("600");
  // Retention was never touched and must still be inherited. A form that posts
  // everything it is showing turns every default into a decision nobody made,
  // and the project stops receiving an improved default forever after.
  await expect(page.getByLabel("Retention for error in days")).toHaveValue("");

  // And emptying the field is how you give the decision back.
  await page.getByLabel("Rate limit per minute").fill("");
  await page.getByRole("button", { name: "Save" }).click();
  await page.reload();
  await expect(page.getByLabel("Rate limit per minute")).toHaveValue("");
});

test("the dashboard answers how bad it is, and where", async () => {
  await page.goto(`/projects/${projectID()}`);

  await expect(page.getByRole("heading", { name: "Events per hour" })).toBeVisible();
  // The range the server actually used, echoed back — the ends are rounded to
  // the hour the buckets are kept in, and a chart labelled with the range it
  // asked for instead of the one it got is off by up to an hour.
  await expect(page.getByText(/over 24 hours/)).toBeVisible();

  // The legend names every severity in words next to its swatch. A red column
  // means nothing to a reader who cannot see it is red.
  await expect(page.getByText(/^error$/).first()).toBeVisible();

  await expect(page.getByRole("heading", { name: /^Loudest issues/ })).toBeVisible();
  await expect(page.getByRole("link", { name: /ValueError/ }).first()).toBeVisible();

  await expect(page.getByRole("heading", { name: /^By release/ })).toBeVisible();
  await expect(page.getByRole("heading", { name: /^By environment/ })).toBeVisible();
  await expect(page.getByText("ui-smoke@1.0.0").first()).toBeVisible();

  // The window is a control on one screen, not an address: pressing it must
  // not put three chart widths in the back button's way.
  const before = page.url();
  await page.getByRole("button", { name: "7 days" }).click();
  await expect(page.getByText(/over 168 hours/)).toBeVisible();
  expect(page.url()).toBe(before);

  // And a breakdown row is a way into the list, which is the whole reason to
  // draw it rather than print it.
  await page
    .getByRole("button", { name: "Filter issues by production" })
    .click();
  await expect(page).toHaveURL(/environment=production/);
  await expect(page.getByRole("link", { name: /ValueError/ })).toBeVisible();
});

test("a search the index cannot run says so, instead of finding nothing", async () => {
  await page.goto(`/projects/${projectID()}/issues`);

  const search = page.getByLabel("Search issues by title");
  await search.fill("ValueErr");
  await expect(page.getByRole("link", { name: /ValueError/ })).toBeVisible();

  await search.fill("Nothingatallliketh");
  await expect(page.getByText(/No issue title contains/)).toBeVisible();

  // The distinction the whole message exists for. Two characters is shorter
  // than the index's window, so the query never ran — and answering it with
  // "no results" is how somebody spends an afternoon looking for a bug in
  // their own data.
  await search.fill("Va");
  const complaint = page.getByRole("alert");
  await expect(complaint).toContainText(/shorter than 3 characters/);
  await expect(complaint).toContainText(/"Va"/);
  await expect(page.getByText(/No issue title contains/)).toHaveCount(0);

  await search.fill("");
  await expect(page.getByRole("link", { name: /ValueError/ })).toBeVisible();
});

test("a tag is a way into the list, not just a fact on a page", async () => {
  await page.goto(`/projects/${projectID()}/issues`);
  await page.getByRole("link", { name: /ValueError/ }).click();

  // Every tag value links to the listing narrowed to it. Before this, a tag
  // could be read one issue at a time and never asked of the list — which is
  // the difference between a fact and a question.
  await page.getByRole("link", { name: "Show issues tagged server: web-01" }).click();

  await expect(page).toHaveURL(/tag=server%3Aweb-01/);
  await expect(page.getByRole("link", { name: /ValueError/ })).toBeVisible();
  // The filter is visible and removable, so nobody is left wondering why the
  // list is short.
  await expect(
    page.getByRole("button", { name: /Remove the server:web-01 filter/ }),
  ).toBeVisible();
});

// The exit criterion of the whole phase, written as the sentence the plan
// uses: "is this new or did it come back, and which release brought it" must
// be answerable from the panel alone. Nothing below touches the CLI or the
// API — every fact is read off the screen.
test("new, or back? and which release brought it — from the panel alone", async () => {
  // A bug that has never been seen before.
  expect(ingest("1".repeat(32), { type: "PaymentError" })).toBe(200);

  await page.goto(`/projects/${projectID()}/issues`);
  await page.getByRole("link", { name: /PaymentError/ }).click();

  await expect(page.getByRole("heading", { name: "PaymentError" })).toBeVisible();
  await expect(page.getByText(/First seen/)).toBeVisible();
  await expect(
    page.getByRole("link", { name: "ui-smoke@1.0.0" }).first(),
  ).toBeVisible();

  // Resolved as of the build it is being seen in, which is what somebody who
  // has just written the fix actually means.
  await page.getByRole("button", { name: "Resolve in next release" }).click();
  await expect(
    page.getByText(/Resolved in the next release after/),
  ).toBeVisible();

  // A straggler from the release it was resolved in. The pods running the old
  // build have not been replaced yet, and treating this as proof the fix
  // failed is the false positive the feature exists to remove.
  expect(ingest("2".repeat(32), { type: "PaymentError" })).toBe(200);
  await page.reload();
  await expect(
    page.getByText(/Resolved in the next release after/),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Reopen" })).toBeVisible();
  // And the suppressed event is shown rather than swallowed: without this the
  // product would look like it had lost it.
  await expect(page.getByText(/arrived so far and did not/)).toBeVisible();

  // Now the fix ships, and the bug is still there.
  expect(
    ingest("3".repeat(32), { type: "PaymentError", release: "ui-smoke@1.1.0" }),
  ).toBe(200);
  await page.reload();

  // The two sentences the phase was built to produce, in words, on the screen.
  await expect(page.getByText(/^Regression/)).toBeVisible();
  await expect(
    page.getByText(/this was resolved and came back/),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "ui-smoke@1.1.0" }).first(),
  ).toBeVisible();
  await expect(page.getByText(/first seen in/)).toBeVisible();
});

test("the release screen says what that deploy did", async () => {
  await page.goto(`/projects/${projectID()}/releases`);

  await expect(page.getByRole("link", { name: /ui-smoke@1\.1\.0/ })).toBeVisible();
  await page.getByRole("link", { name: /ui-smoke@1\.1\.0/ }).click();

  // One issue came back in this release and none was new in it: the release
  // that broke something is the one that says so.
  const regressed = page.locator("div", { hasText: /^Regressed/ }).last();
  await expect(regressed).toContainText("1");
  await expect(page.getByText("came back in this release")).toBeVisible();
  await expect(page.getByText("first seen in this release")).toBeVisible();
  await expect(page.getByText("from the hourly buckets")).toBeVisible();

  // No deploy tool ran, so there are no commits — and the screen says what
  // would put them there rather than showing an empty box.
  await expect(page.getByText(/No commits associated/)).toBeVisible();
});

test("which change caused it, from the commits the deploy sent", async () => {
  // The other half of ADR 019, on the screen. The commit set arrives the way a
  // deploy pipeline sends it — through the API, with a patch set — because
  // what is under test is the panel, and driving `git` from a browser test
  // would only test this file's idea of a repository.
  //
  // `app/views.py` is the file the frame of every event in this run names, so
  // exactly one of these three commits can be the suspect.
  const sent = await page.request.post(
    `/api/v1/projects/${projectID()}/releases/ui-smoke%401.0.0/commits`,
    {
      headers: { "X-Trapline-Request": "1" },
      data: {
        commits: [
          {
            id: "a".repeat(40),
            message: "rewrite the checkout view",
            author_name: "Antonio Vila",
            patch_set: [{ path: "app/views.py", type: "M" }],
          },
          {
            id: "b".repeat(40),
            message: "bump a dependency",
            author_name: "Antonio Vila",
            patch_set: [{ path: "requirements.txt", type: "M" }],
          },
          {
            id: "c".repeat(40),
            message: "update the readme",
            author_name: "Antonio Vila",
            patch_set: [{ path: "README.md", type: "M" }],
          },
        ],
      },
    },
  );
  expect(sent.status()).toBe(200);

  // The ValueError issue: first seen in ui-smoke@1.0.0, which is the release
  // those commits belong to.
  await page.goto(`/projects/${projectID()}/issues`);
  await page.getByRole("link", { name: /ValueError/ }).click();

  await expect(
    page.getByRole("heading", { name: /Suspect commits/ }),
  ).toBeVisible();
  await expect(page.getByText("rewrite the checkout view")).toBeVisible();
  // The accusation comes with its evidence, or it is just a commit with a
  // highlight around it.
  await expect(page.getByText(/touched.*app\/views\.py/)).toBeVisible();
  await expect(page.getByText(/frame #1/)).toBeVisible();
  // And the commits that touched nothing in the stacktrace are not accused.
  await expect(page.getByText("bump a dependency")).toHaveCount(0);
  await expect(page.getByText("update the readme")).toHaveCount(0);
});

test("the live feed offers new issues instead of moving the list", async () => {
  await page.goto(`/projects/${projectID()}/issues`);
  await expect(page.getByRole("link", { name: /ValueError/ })).toBeVisible();

  const before = await page.getByRole("listitem").count();

  // Something breaks while somebody is reading. The rows must not reorder
  // under them — during an incident that moves the line they were about to
  // click — so the arrival waits behind a banner.
  expect(ingest("4".repeat(32), { type: "TimeoutError" })).toBe(200);

  const banner = page.getByRole("button", { name: /1 new issue/ });
  await expect(banner).toBeVisible();
  await expect(page.getByRole("listitem")).toHaveCount(before);

  await banner.click();
  await expect(page.getByRole("link", { name: /TimeoutError/ })).toBeVisible();
  await expect(banner).toHaveCount(0);
});

// The exit criterion of the whole alerting phase, and the sentence the plan
// uses: break an application on purpose and be told about it, with a link to
// the issue, in under a minute, without touching anything.
//
// "Without touching anything" is what makes this a browser test rather than a
// shell one. Every part of the configuration below is typed into the panel —
// the channel, its signing secret, the rule and the channel it fans out to —
// and the only thing that happens outside the browser is the application
// breaking, which is the one part a user does not do from a screen.
//
// The far end is scripts/ui-smoke.sh's receiver: a separate process on the
// same network that verifies the HMAC with code importing nothing from this
// project. A notification the server delivered to itself would prove nothing.

const RECEIVER = process.env["TRAPLINE_RECEIVER_URL"];
const WEBHOOK_SECRET = process.env["TRAPLINE_WEBHOOK_SECRET"];
const CHANNEL = "the gate";
const RULE = "anything new";

/** deliveries reads back everything the receiver has been sent. */
async function deliveries(): Promise<
  { channel: string; body: string; signed: boolean | null }[]
> {
  const response = await fetch(`${RECEIVER}/deliveries`);
  if (!response.ok) throw new Error(`the receiver answered ${response.status}`);
  const payload = (await response.json()) as {
    deliveries: { channel: string; body: string; signed: boolean | null }[];
    bad: number;
  };
  if (payload.bad !== 0) {
    throw new Error(`the receiver rejected ${payload.bad} signatures`);
  }
  // Null rather than an empty array when nothing has arrived: the receiver
  // encodes a Go nil slice, and "nothing yet" is the state this polls from.
  return payload.deliveries ?? [];
}

/**
 * waitForDelivery polls until a notification mentioning `marker` arrives.
 *
 * The budget is the claim: sixty seconds from the application breaking to
 * somebody being told. It is not a generous timeout that happens to pass —
 * exceeding it is the feature not working.
 */
async function waitForDelivery(
  marker: string,
  budgetMs: number,
): Promise<{ body: string; elapsedMs: number }> {
  const started = Date.now();
  for (;;) {
    for (const one of await deliveries()) {
      if (one.channel === "webhook" && one.body.includes(marker)) {
        if (one.signed !== true) {
          throw new Error("the delivery carried no verified signature");
        }
        return { body: one.body, elapsedMs: Date.now() - started };
      }
    }
    if (Date.now() - started > budgetMs) {
      throw new Error(
        `no notification about ${marker} arrived within ${budgetMs / 1000}s`,
      );
    }
    await new Promise((resume) => setTimeout(resume, 250));
  }
}

test("alerting is configured from the panel, secrets included — and not read back", async () => {
  expect(
    RECEIVER,
    "TRAPLINE_RECEIVER_URL is required; run this through scripts/ui-smoke.sh",
  ).toBeTruthy();
  expect(WEBHOOK_SECRET).toBeTruthy();

  // Reachable from every screen, because the moment somebody wants it is the
  // moment they are three clicks deep in an incident.
  await page.getByRole("link", { name: "Alerts" }).first().click();
  await expect(page.getByRole("heading", { name: "Channels" })).toBeVisible();

  // With nothing configured, the panel says what that costs rather than
  // showing an empty box.
  await expect(page.getByText(/No channels yet/)).toBeVisible();
  await expect(
    page.getByText(/The notifier is not running, because no channel/),
  ).toBeVisible();

  await page.getByLabel("Channel type").selectOption("webhook");
  await page.getByLabel("Channel name").fill(CHANNEL);
  await page.getByLabel("Endpoint URL").fill(`${RECEIVER}/hook`);
  await page.getByLabel("Signing secret").fill(WEBHOOK_SECRET ?? "");
  await page.getByRole("button", { name: "Save channel" }).click();

  await expect(page.getByRole("status")).toContainText("stored encrypted");

  // The credential goes in and does not come back. The encryption at rest
  // buys nothing if the screen that wrote it echoes it: the leak would simply
  // move from the file to the front door.
  await expect(page.getByText(CHANNEL, { exact: false }).first()).toBeVisible();
  const secret = String(WEBHOOK_SECRET);
  expect(await page.content()).not.toContain(secret);
  await page.reload();
  await expect(page.getByRole("heading", { name: "Channels" })).toBeVisible();
  expect(await page.content()).not.toContain(secret);

  // And the subsystem is running now, without a restart, while the person who
  // configured it is still looking at the screen.
  await expect(
    page.getByText(/The notifier is running/),
  ).toBeVisible();
});

test("both families of monitor live on one screen, and the public page is a click away", async () => {
  await page.goto(`/projects/${projectID()}/monitors`);
  await expect(page.getByRole("heading", { name: "Monitors" })).toBeVisible();

  // One screen with both, which is the point of ADR 037: an operator asking
  // "what is this installation watching" must not have to know which of the
  // two words the feature was filed under.
  await expect(page.getByRole("heading", { name: "Uptime" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Cron" })).toBeVisible();

  // A cron monitor, because it is the half that needs no outbound connection:
  // the uptime half is exercised against a container that gets switched off in
  // scripts/uptime.sh, which is the only honest way to test it.
  await page.getByRole("heading", { name: "Cron" }).scrollIntoViewIfNeeded();
  const cron = page.locator("section").filter({ hasText: "A job that is supposed to report in" });
  await cron.getByLabel("Name").fill("nightly-backup");
  await cron.getByLabel("Schedule").fill("0 3 * * *");
  await cron.getByRole("button", { name: "Add" }).click();

  await expect(page.getByText("nightly-backup")).toBeVisible();
  // The ping URL is on the screen because the whole feature is a URL somebody
  // pastes into a crontab; a key you cannot read is a monitor you cannot use.
  await expect(page.getByText(/\/ping\//).first()).toBeVisible();
  await expect(page.getByText("0 3 * * * (UTC)")).toBeVisible();
  await expect(page.getByText("never checked in")).toBeVisible();

  // And the public page's switch is on the same screen as the monitors it
  // publishes, rather than buried in project settings.
  await expect(
    page.getByRole("heading", { name: "Public status page" }),
  ).toBeVisible();
  // Clicked rather than check()/uncheck(): the box is controlled by what the
  // server last said, so it only moves once the write has landed and the
  // screen has reloaded. Playwright's check() asserts the state changed the
  // instant it clicks, which is one round trip too early.
  const publish = page.getByRole("checkbox", { name: /Publish this project/ });
  await expect(publish).not.toBeChecked();
  await publish.click();
  await expect(publish).toBeChecked();
  await expect(page.getByRole("link", { name: "Open the page" })).toBeVisible();

  await page.getByLabel("Title").fill("Estado de ui-smoke");
  await page.getByLabel("Description").fill("servicios en vivo");
  await page
    .locator("section")
    .filter({ hasText: "A page anybody can read" })
    .getByRole("button", { name: "Save" })
    .click();

  // The page itself: no session, no script, and the heading that was just
  // typed. A separate context so it is read the way a stranger reads it —
  // there is no cookie in it.
  const stranger = await page.context().browser()!.newContext();
  const public_ = await stranger.newPage();
  const response = await public_.goto(`/status/ui-smoke`);
  expect(response?.status()).toBe(200);
  expect(response?.headers()["cache-control"]).toContain("max-age=30");
  await expect(public_.getByRole("heading", { name: "Estado de ui-smoke" })).toBeVisible();
  await expect(public_.getByText("servicios en vivo")).toBeVisible();
  // No uptime monitor is marked public, so it says so rather than claiming
  // everything is fine about nothing.
  await expect(public_.getByText("Nothing is being reported on yet")).toBeVisible();
  expect(await public_.locator("script").count()).toBe(0);
  await stranger.close();

  // Switching it off takes effect now, not when a thirty-second cache expires.
  await publish.click();
  await expect(publish).not.toBeChecked();
  await expect(page.getByRole("link", { name: "Open the page" })).toHaveCount(0);
  const gone = await page.request.get(`/status/ui-smoke`);
  expect(gone.status()).toBe(404);

  // Back where the story was. The file is one session in one browser and the
  // tests after this one carry on from the alerts screen, so a test that
  // wanders off has to walk back.
  await page.goto("/alerts");
  await expect(page.getByRole("heading", { name: "Delivery log" })).toBeVisible();
});

test("the digest job says why it is not running, and its schedule is editable", async () => {
  // The gap ADR 014 deliberately leaves in /system/jobs — only running jobs
  // appear — closed where somebody would actually look for the answer.
  await expect(
    page.getByText(/Not scheduled: no channel has asked for it/),
  ).toBeVisible();

  await page.getByLabel("Day").selectOption("Thursday");
  await page.getByLabel("Hour").fill("17");
  await page.getByRole("button", { name: "Save schedule" }).click();
  await expect(page.getByRole("status")).toContainText(
    "Thursday at 17:00 UTC",
  );

  await page.reload();
  await expect(page.getByLabel("Day")).toHaveValue("Thursday");
  await expect(page.getByLabel("Hour")).toHaveValue("17");
});

test("break an app on purpose and be told, with a link to the issue, in under a minute", async () => {
  await page.getByLabel("Rule name").fill(RULE);
  // The defaults are the rule most people want: every project, anything new.
  await expect(page.getByLabel("Trigger")).toHaveValue("new_issue");
  await page.getByRole("checkbox", { name: CHANNEL }).check();
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page.getByRole("status")).toContainText(RULE);
  await expect(page.getByText(/a new issue appears/)).toBeVisible();

  // Nothing else is configured. From here the only action is an application
  // failing, which is the whole of the claim.
  const before = (await deliveries()).length;
  expect(before).toBe(0);

  const started = Date.now();
  expect(ingest("5".repeat(32), { type: "CheckoutError" })).toBe(200);

  const { body, elapsedMs } = await waitForDelivery("CheckoutError", 60_000);
  expect(
    Date.now() - started,
    "the alert took longer than the minute the product promises",
  ).toBeLessThan(60_000);
  expect(elapsedMs).toBeLessThan(60_000);

  const payload = JSON.parse(body) as {
    event: string;
    rule: string;
    title: string;
    url: string;
  };
  expect(payload.event).toBe("new_issue");
  expect(payload.rule).toBe(RULE);
  expect(payload.title).toContain("CheckoutError");

  // The link is the difference between an alert and an interruption, so it is
  // followed rather than pattern-matched: a URL that is the right shape and
  // the wrong issue looks identical in a string comparison and is useless to
  // the person it wakes up.
  expect(payload.url).toMatch(
    new RegExp(`^${process.env["TRAPLINE_BASE_URL"]}/projects/\\d+/issues/\\d+$`),
  );
  await page.goto(payload.url);
  await expect(
    page.getByRole("heading", { name: "CheckoutError" }),
  ).toBeVisible();
});

test("the delivery log says what became of it, and tells dead apart from failed", async () => {
  await page.getByRole("link", { name: "Alerts" }).first().click();
  await expect(page.getByRole("heading", { name: "Delivery log" })).toBeVisible();

  // The one that worked.
  await expect(page.getByText("delivered", { exact: false }).first()).toBeVisible();
  await page.getByRole("button", { name: "sent", exact: true }).click();
  await expect(page.getByText(/CheckoutError/).first()).toBeVisible();

  // Now a channel that cannot possibly work: port 1 on a host nothing is
  // listening on. The row it produces is the one an operator has to be able
  // to read.
  await page.getByRole("button", { name: "All" }).click();
  await page.getByLabel("Channel type").selectOption("webhook");
  await page.getByLabel("Channel name").fill("nowhere");
  await page.getByLabel("Endpoint URL").fill("http://127.0.0.1:1/hook");
  await page
    .getByLabel("Signing secret")
    .fill("another-secret-long-enough");
  await page.getByRole("button", { name: "Save channel" }).click();
  await expect(page.getByRole("status")).toContainText("nowhere");

  await page.getByLabel("Rule name").fill("into the void");
  await page.getByRole("checkbox", { name: "nowhere" }).check();
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page.getByRole("status")).toContainText("into the void");

  expect(ingest("6".repeat(32), { type: "DeadLetterError" })).toBe(200);

  // failed, not dead: the notifier will come back to it on its own, and the
  // panel says so in words rather than leaving the reader to know that.
  //
  // The whole block retries, and the sentence is what it waits for rather than
  // the issue's name. The same failure is also delivered successfully to the
  // other channel, so "DeadLetterError is on the screen" is true from the
  // moment the page loads and would pass against the unfiltered list while
  // the attempt this is about has not happened yet. Only a failed row renders
  // this sentence. The log does not poll, so each attempt reloads.
  await expect(async () => {
    await page.reload();
    await page.getByRole("button", { name: "failed", exact: true }).click();
    await expect(
      page.getByText(/it will be retried on its own/).first(),
    ).toBeVisible({ timeout: 3_000 });
    await expect(page.getByText(/DeadLetterError/).first()).toBeVisible({
      timeout: 3_000,
    });
  }).toPass({ timeout: 30_000 });
  await expect(page.getByRole("button", { name: "Retry" }).first()).toBeVisible();

  // Removing the channel closes every delivery queued for it: ten attempts
  // that can only fail is the same outcome reached slowly, with ten
  // misleading log lines. That is a `dead` row, and it must not read like a
  // `failed` one — one of them is still coming and the other is not.
  await page.getByRole("button", { name: "All" }).click();
  // First, because the rule that fans out to it names it too: the channel
  // list is rendered above the rule list.
  await page
    .getByRole("listitem")
    .filter({ hasText: "nowhere" })
    .first()
    .getByRole("button", { name: "Remove" })
    .click();
  await expect(page.getByRole("status")).toContainText("nowhere");

  await page.getByRole("button", { name: "dead", exact: true }).click();
  await expect(page.getByText(/DeadLetterError/).first()).toBeVisible();
  await expect(
    page.getByText(/will NOT be retried unless you ask/).first(),
  ).toBeVisible();
  await expect(page.getByText(/the channel was deleted/).first()).toBeVisible();
});

// Tracing and release health, which are the two screens observability exists for.
//
// They come last in this story for one reason: switching tracing and sessions
// on means naming the enabled categories explicitly, and every earlier test
// depends on this project still inheriting the default profile.

const TRACED = "GET /api/checkout";
const HEALTH_RELEASE = "ui-smoke@1.1.0";

test("the performance table says which endpoint is slow, and how it knows", async () => {
  // Tracing is off until a project asks for it, which is the whole of ADR 005.
  // Sampling goes to 1 so every waterfall is stored: the screen under test is
  // the one that opens a trace, and a sampled-away trace is a passing test
  // that proved nothing.
  const configured = await page.request.put(
    `/api/v1/projects/${projectID()}/config`,
    {
      headers: { "X-Trapline-Request": "1" },
      data: {
        enabled_categories: ["error", "transaction", "session"],
        traces_sample_rate: 1,
      },
    },
  );
  expect(configured.status()).toBe(200);

  // Thirty requests with a spread of durations and one failure, so the three
  // percentiles are three different numbers and the failure column has
  // something in it. One in thirty is 3.3%, which is also the check that the
  // column is a share and not a count.
  for (let index = 0; index < 30; index++) {
    expect(
      sendTransaction({
        name: TRACED,
        durationMS: 40 + index * 20,
        failed: index === 7,
      }),
    ).toBe(200);
  }

  await page.goto(`/projects/${projectID()}/performance`);

  await expect(page.getByRole("columnheader", { name: "p50" })).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "p95" })).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "p99" })).toBeVisible();
  await expect(page.getByRole("link", { name: new RegExp(TRACED) })).toBeVisible();

  // The sampling line. A reader who is shown a count and a number of stored
  // traces without being told which was sampled will compare one of them
  // against their load balancer and conclude this product loses data
  // (ADR 021).
  await expect(page.getByText(/30 transactions/)).toBeVisible();
  await expect(page.getByText(/1 failed \(3\.3%\)/)).toBeVisible();
  await expect(page.getByText(/30 waterfalls kept, 100% of 30 received/)).toBeVisible();

  // Busiest and slowest are the same one row here; what is being checked is
  // that the control re-asks rather than sorting what it already has, because
  // a client-side sort of a truncated page is a ranking of the wrong set.
  await page.getByRole("button", { name: "Busiest" }).click();
  await expect(page.getByRole("link", { name: new RegExp(TRACED) })).toBeVisible();
  await page.getByRole("button", { name: "Slowest" }).click();
});

test("opening a transaction gives its history and a waterfall to click", async () => {
  await page.getByRole("link", { name: new RegExp(TRACED) }).click();

  // The address carries the transaction, because "this endpoint got slower
  // after the deploy" is a link somebody pastes into a chat.
  await expect(page).toHaveURL(/\/performance\/GET%20%2Fapi%2Fcheckout$/);

  // The chart says it in pixels; this says it in words, which is the same
  // rule every other chart in this panel follows.
  await expect(page.getByText(/half the requests finished within/)).toBeVisible();
  await expect(page.getByText(/never a saved percentile/)).toBeVisible();
  await expect(page.getByText("p99", { exact: true }).first()).toBeVisible();

  await expect(
    page.getByRole("heading", { name: "Slowest kept traces" }),
  ).toBeVisible();
  await page.getByRole("link", { name: /Open the waterfall/ }).first().click();

  // The waterfall, whole: the root span and the child the sender attached. A
  // trace is stored entire or not at all, so half a waterfall would be a bug
  // rather than sampling (ADR 021).
  await expect(page.getByRole("heading", { name: /Waterfall/ })).toBeVisible();
  await expect(page.getByText("http.server").first()).toBeVisible();
  await expect(page.getByText("db.sql").first()).toBeVisible();
  await expect(page.getByText("SELECT 1").first()).toBeVisible();

  // And the way back is the transaction, not the browser's back button.
  await page.getByRole("link", { name: TRACED }).click();
  await expect(page).toHaveURL(/\/performance\/GET%20%2Fapi%2Fcheckout$/);
});

test("sessions from an SDK that had been offline are accepted", async () => {
  // The protocol's own `sessions` item: counts a client already added up,
  // which is what it sends when it has been offline and is reporting what
  // happened while it was. Twenty sessions, one of them a crash — 95%
  // crash-free, a number the second half of this gate reads off the screen
  // after a restart has drained the window (ADR 008).
  const hour = new Date();
  hour.setUTCMinutes(0, 0, 0);
  const aggregate = JSON.stringify({
    attrs: { release: HEALTH_RELEASE, environment: "production" },
    aggregates: [
      { started: hour.toISOString(), exited: 17, errored: 2, crashed: 1 },
    ],
  });
  expect(postItem("sessions", aggregate)).toBe(200);

  // Nothing is on the screen yet, and that is the point: the counters live in
  // a bounded window in memory until they are flushed, so what this asserts is
  // the honest "not yet" rather than a number that has not been earned.
  const health = await page.request.get(
    `/api/v1/projects/${projectID()}/health`,
  );
  expect(health.status()).toBe(200);
  expect((await health.json()).started).toBe(0);
});

// The theme is checked in both directions because it has already been wrong in
// one: Tailwind v4 hoists every @theme block it finds into a single
// unconditional :root rule, so a dark ramp nested in a media query shipped to
// everybody and the light one never shipped at all. Nothing in the type
// checker, the unit tests or the API suite can see that — only a browser told
// what its reader prefers.
for (const scheme of ["light", "dark"] as const) {
  test(`the panel respects prefers-color-scheme: ${scheme}`, async ({
    browser,
  }) => {
    const themed = await browser.newContext({
      colorScheme: scheme,
      storageState: await context.storageState(),
    });
    const themedPage = await themed.newPage();
    await themedPage.goto("/");
    await expect(
      themedPage.getByRole("button", { name: "Sign out" }),
    ).toBeVisible();

    // Through a canvas rather than by parsing the string. The palette is
    // written in oklch and Chromium hands back the computed value in the
    // colour space it was authored in, so `oklch(0.99 0.002 260)` read as
    // three numbers looks almost black — a measurement that would have failed
    // a page that was perfectly correct, which is worse than not measuring.
    const { background, text, surface, ink } = await themedPage.evaluate(() => {
      const style = getComputedStyle(document.body);
      const luminance = (colour: string): number => {
        const canvas = document.createElement("canvas");
        canvas.width = 1;
        canvas.height = 1;
        const context = canvas.getContext("2d");
        if (!context) throw new Error("no 2d canvas context");
        context.fillStyle = "#000000";
        // An unparseable value leaves fillStyle alone, so it stays black and
        // the contrast assertion below fails loudly instead of passing on a
        // number nobody computed.
        context.fillStyle = colour;
        context.fillRect(0, 0, 1, 1);
        const [r = 0, g = 0, b = 0] = context.getImageData(0, 0, 1, 1).data;
        return (0.2126 * r + 0.7152 * g + 0.0722 * b) / 255;
      };
      return {
        background: style.backgroundColor,
        text: style.color,
        surface: luminance(style.backgroundColor),
        ink: luminance(style.color),
      };
    });

    if (scheme === "light") {
      expect(surface, `light background was ${background}`).toBeGreaterThan(0.8);
      expect(ink, `light text was ${text}`).toBeLessThan(0.4);
    } else {
      expect(surface, `dark background was ${background}`).toBeLessThan(0.3);
      expect(ink, `dark text was ${text}`).toBeGreaterThan(0.6);
    }

    // Whatever the scheme, the page has to be readable at all. A background
    // and a foreground that agree is the exact shape the hoisting bug took.
    expect(Math.abs(surface - ink)).toBeGreaterThan(0.4);

    await themed.close();
  });
}
