// The API client.
//
// Relative URLs everywhere: in production the panel is served by the same
// process that answers these calls, and in development Vite proxies /api to a
// local server. One code path, no environment-dependent base URL to get wrong.

import type { EventPayload } from "./event";

const BASE = "/api/v1";

// The server requires this header on state-changing requests as its CSRF
// defence: a browser cannot set a custom header cross-origin without a
// preflight, and no permissive CORS policy is served for one to pass.
const CSRF_HEADER = "X-Trapline-Request";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }

  get isUnauthorized(): boolean {
    return this.status === 401;
  }

  /**
   * A search the index cannot run, as opposed to one that found nothing.
   *
   * The two look identical on a screen that treats them the same, and the
   * difference is an afternoon: "no results" for a query that never ran sends
   * somebody looking for a bug in their own data. The server says 400 and says
   * why (ADR 011); this is how the screen can tell.
   */
  get isUnaskable(): boolean {
    return this.status === 400;
  }
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const response = await fetch(BASE + path, {
    method,
    headers: {
      "Content-Type": "application/json",
      [CSRF_HEADER]: "1",
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  if (!response.ok) {
    // The API returns one error shape for every failure, so this needs no
    // per-endpoint special casing.
    let message = `request failed (${response.status})`;
    try {
      const payload = (await response.json()) as { error?: string };
      if (payload.error) message = payload.error;
    } catch {
      // A body that is not the documented shape is not worth reporting over
      // the status code, which is always meaningful.
    }
    throw new ApiError(response.status, message);
  }

  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

/**
 * A GET whose body is a document rather than a record.
 *
 * One endpoint needs it: the issue bundle is markdown, and decoding it as JSON
 * would fail on the first character. It shares the error handling with
 * request() — one shape for every failure — so the screen above it does not
 * have to special-case the one call that is not JSON.
 */
async function requestText(path: string, accept: string): Promise<string> {
  const response = await fetch(BASE + path, {
    method: "GET",
    headers: { Accept: accept, [CSRF_HEADER]: "1" },
  });

  if (!response.ok) {
    let message = `request failed (${response.status})`;
    try {
      const payload = (await response.json()) as { error?: string };
      if (payload.error) message = payload.error;
    } catch {
      // Not the documented error shape; the status code still means something.
    }
    throw new ApiError(response.status, message);
  }
  return await response.text();
}

export interface Key {
  public_key: string;
  dsn: string;
  created_at: string;
}

export interface Project {
  id: number;
  name: string;
  /** What a deploy tool puts in a URL: SENTRY_PROJECT takes this (ADR 013). */
  slug: string;
  created_at: string;
  dsn: string;
  keys: Key[];
}

export interface Admin {
  id: number;
  username: string;
}

export type Level = "fatal" | "error" | "warning" | "info" | "debug";
export type IssueStatus = "unresolved" | "resolved" | "ignored";

export interface Issue {
  id: number;
  project_id: number;
  fingerprint: string;
  title: string;
  culprit: string;
  level: Level;
  status: IssueStatus;
  first_seen: string;
  last_seen: string;
  times: number;
  last_release?: string;
}

export interface StoredEvent {
  id: number;
  event_id: string;
  occurred_at: string;
  received_at: string;
  level: Level;
  release?: string;
  environment?: string;
  message?: string;
  payload: EventPayload;
}

export interface TagCount {
  value: string;
  count: number;
}

export interface IssueDetail extends Issue {
  events: StoredEvent[];
  tags: Record<string, TagCount[]>;
  /** The release the issue's first event carried. */
  first_release?: string;
  resolved_at?: string;
  /** The release it was being seen in when somebody resolved it. */
  resolved_in_release?: string;
  /** With this on, only a newer release counts as a regression (ADR 012). */
  resolve_next_release: boolean;
  regressions: number;
  /** Which deploy brought it back — the other half of "new, or back?". */
  regressed_in_release?: string;
  /**
   * Events counted from the release it was resolved in, which did not reopen
   * it. Shown rather than swallowed: without it the product looks like it lost
   * them.
   */
  seen_in_resolved_release_count: number;
}

/** Just enough of a project to title the screen showing its issues. */
export interface ProjectRef {
  id: number;
  name: string;
}

export interface IssuePage {
  issues: Issue[];
  next_cursor?: string;
  /** Counts cover the whole project, not the current filter, so the numbers on
   *  the filter buttons do not change depending on which one is pressed. */
  counts: Record<string, number>;
  /** The project these issues belong to, so the heading does not cost a second
   *  request that can fail on its own or land after the rows it labels. */
  project: ProjectRef;
}

export interface IssueFilter {
  status?: IssueStatus;
  q?: string;
  environment?: string;
  release?: string;
  /** An arbitrary tag, as `key:value`. How "only on Safari?" is asked. */
  tag?: string;
  cursor?: string;
  limit?: number;
}

function issueQuery(filters: IssueFilter): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (value !== undefined && value !== "") params.set(key, String(value));
  }
  const query = params.toString();
  return query ? `?${query}` : "";
}

/**
 * A project's ingest profile.
 *
 * Explicit settings and defaults arrive separately rather than merged, so an
 * unset value stays unset: saving a merged view would turn every default into
 * a decision nobody made, and raising a default later would never reach the
 * projects that never chose otherwise.
 */
export interface ProjectConfig {
  /** Null when nothing is set, so the defaults apply. An empty array is the
   *  opposite state and not the same thing: it means the operator switched
   *  every category off and the project accepts nothing. */
  enabled_categories: string[] | null;
  /** Zero means unset. */
  rate_limit_per_minute: number;
  /** A category missing from the map, or present as zero, is unset. */
  retention_days: Record<string, number>;
  defaults: {
    enabled_categories: string[];
    rate_limit_per_minute: number;
    retention_days: Record<string, number>;
  };
  available_categories: string[];
  /** Whether this project publishes a public page at /status/<slug>. */
  status_page: { enabled: boolean };
}

/**
 * A change to a project's ingest profile.
 *
 * Omitting a field leaves it alone. Sending null clears it back to the default
 * — and for the category list that is the only way back, because an empty
 * array already means "accept nothing". Zero does the same for the two
 * numbers.
 */
export interface ProjectConfigChanges {
  enabled_categories?: string[] | null;
  rate_limit_per_minute?: number | null;
  retention_days?: Record<string, number> | null;
  status_page?: { enabled: boolean } | null;
}

/** A cron monitor: something that is supposed to report in (ADR 016). */
export interface CronMonitor {
  id: number;
  project_id: number;
  slug: string;
  ping_key: string;
  ping_url: string;
  schedule_type: "crontab" | "interval";
  schedule: string;
  timezone: string;
  checkin_margin_s: number;
  max_runtime_s: number;
  status: "unknown" | "ok" | "missed" | "timeout" | "error";
  last_checkin_at?: string;
  next_expected_at?: string;
  enabled: boolean;
  created_at: string;
}

/** One reported run of a cron monitor. */
export interface CronCheckIn {
  id: number;
  monitor_id: number;
  checkin_id?: string;
  status: "in_progress" | "ok" | "error";
  started_at: string;
  finished_at?: string;
  duration_ms: number;
  environment?: string;
}

/** An uptime monitor: something this server asks (ADR 016). */
export interface UptimeMonitor {
  id: number;
  project_id: number;
  name: string;
  url: string;
  method: "GET" | "HEAD";
  interval_s: number;
  timeout_s: number;
  expected_status_min: number;
  expected_status_max: number;
  expected_body_substring?: string;
  follow_redirects: boolean;
  allow_private: boolean;
  public: boolean;
  enabled: boolean;
  status: "unknown" | "up" | "down";
  consecutive_failures: number;
  last_checked_at: string | null;
  next_check_at: string;
  last_status_change_at: string | null;
  created_at: string;
}

/** One check of an uptime monitor. */
export interface UptimeResult {
  at: string;
  ok: boolean;
  status_code: number;
  latency_ms: number;
  error?: string;
}

/**
 * One day of a monitor's history.
 *
 * The ninety-day bar is drawn from these and never from the checks
 * themselves: ninety days of a sixty-second monitor is a hundred and thirty
 * thousand rows, and a screen that reads them is the query ADR 001 exists to
 * forbid.
 */
export interface UptimeDay {
  day: string;
  checks: number;
  failures: number;
  uptime: number;
  mean_latency_ms: number;
}

/** The heading on every public status page. */
export interface StatusPageSettings {
  title: string;
  description: string;
}

/** A named window the stats endpoints understand. */
export type StatsWindow = "24h" | "14d";

/** How far back the dashboard is looking. */
export type DashboardRange = "24h" | "7d" | "30d";

/** The hours a range covers, echoed back because the ends were rounded. */
export interface RangeBounds {
  from: string;
  to: string;
  hours: number;
}

export interface HourlyPoint {
  hour: string;
  count: number;
  by_level: Record<string, number>;
}

export interface ProjectSeries {
  project: ProjectRef;
  range: RangeBounds;
  total: number;
  series: HourlyPoint[];
  by_level: Record<string, number>;
}

export interface TopIssue extends Issue {
  /** Events in the range, which is not the issue's lifetime `times`. */
  count: number;
}

export interface TopIssues {
  project: ProjectRef;
  range: RangeBounds;
  issues: TopIssue[];
}

export interface Breakdown {
  project: ProjectRef;
  range: RangeBounds;
  by: string;
  values: { value: string; count: number }[];
}

export interface IssueSeries {
  issue_id: number;
  range: StatsWindow;
  range_bounds: RangeBounds;
  total: number;
  series: { hour: string; count: number }[];
}

export interface Sparklines {
  project: ProjectRef;
  range: StatsWindow;
  range_bounds: RangeBounds;
  hours: string[];
  issues: { issue_id: number; total: number; counts: number[] }[];
}

export interface Release {
  version: string;
  project_id: number;
  created_at: string;
  /** Null while it is still being deployed. */
  date_released: string | null;
  first_event_at: string | null;
  last_event_at: string | null;
  commit_count: number;
}

export interface ReleaseCommit {
  id: string;
  message?: string;
  author_name?: string;
  author_email?: string;
  timestamp?: string;
  repository?: string;
  patch_set?: { path: string; type: string }[];
}

export interface ReleaseDeploy {
  id: number;
  environment: string;
  name?: string;
  url?: string;
  started_at?: string;
  finished_at?: string;
}

export interface ReleaseDetail extends Release {
  new_issues: number;
  regressed_issues: number;
  events: number;
  commits: ReleaseCommit[];
  deploys: ReleaseDeploy[];
}

/** Why a commit is a suspect: what it touched, and which frame that is. */
export interface SuspectReason {
  /** The file the commit touched, as the repository spells it. */
  path: string;
  /** A, M or D — the protocol's own vocabulary for a change. */
  type: string;
  /** The file the frame named, which is usually spelled differently. */
  frame_path: string;
  /** 0 is the call that actually raised. */
  frame_depth: number;
  /** How many trailing path segments matched: the strength of the evidence. */
  segments: number;
}

export interface Suspect extends ReleaseCommit {
  /** Orders this list and means nothing outside it (ADR 019). */
  score: number;
  reasons: SuspectReason[];
}

/**
 * Which change probably caused an issue.
 *
 * The empty answers matter as much as the full one: no commit set, no patch
 * set, and "these frames are still minified" are each something the reader
 * can go and fix, and `warning` is where that is said (ADR 019).
 */
export interface SuspectsReport {
  /** The release whose commits were considered: the issue's first. */
  release?: string;
  suspects: Suspect[];
  /** The candidates, bounded — what was ruled out. */
  commits: ReleaseCommit[];
  commit_count: number;
  warning?: string;
  /** Whether the frames considered had been resolved from a source map. */
  symbolicated: boolean;
}

/** One uploaded script or source map (ADR 018). */
export interface Artifact {
  id: number;
  project_id: number;
  release_id: number | null;
  dist: string;
  /** Empty for an artefact uploaded the old way, found by release and url. */
  debug_id: string;
  /** Which upload this file arrived in. Nothing is looked up by it. */
  bundle_debug_id: string;
  name: string;
  kind: "minified_source" | "source_map";
  /** The `sourcemap` header of a script: what joins it to its map. */
  sourcemap_ref: string;
  sha256: string;
  size: number;
  created_at: string;
}

export interface ArtifactList {
  artifacts: Artifact[];
  /** What the project holds and what it may hold, in bytes. */
  used_bytes: number;
  budget_bytes: number;
}

/**
 * rangeFor turns the dashboard's three buttons into the two instants the API
 * takes.
 *
 * Computed on the client rather than named on the wire, because the API's
 * range is deliberately two arbitrary ends — a chart of last Tuesday is the
 * same endpoint — and a server-side alias for three of them would be a second
 * vocabulary to keep in step with this one.
 */
export function rangeFor(range: DashboardRange): { from: string; to: string } {
  const hours = range === "24h" ? 24 : range === "7d" ? 24 * 7 : 24 * 30;
  const to = new Date();
  const from = new Date(to.getTime() - (hours - 1) * 3600_000);
  return { from: from.toISOString(), to: to.toISOString() };
}

function statsQuery(values: Record<string, string | number | undefined>): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    if (value !== undefined && value !== "") params.set(key, String(value));
  }
  const query = params.toString();
  return query ? `?${query}` : "";
}


/** The five destinations a notification can be delivered to (ADR 015). */
export type ChannelType = "telegram" | "slack" | "discord" | "webhook" | "email";

/**
 * A channel's configuration, as it goes out and as it comes back — which are
 * not the same document.
 *
 * Everything secret is write-only: a bot token, a webhook signing secret and
 * an SMTP password go in and are never returned. That is not a UI nicety, it
 * is the reason the values are encrypted at rest at all; a listing that
 * echoed them would leak the credential through the front door instead of the
 * file. For Slack and Discord the *path* of the incoming webhook is itself
 * the credential, so what comes back is only its origin.
 */
export interface ChannelConfig {
  bot_token?: string;
  chat_id?: string;
  url?: string;
  secret?: string;
  base_url?: string;
  host?: string;
  port?: number;
  username?: string;
  password?: string;
  from?: string;
  to?: string[];
  starttls?: boolean;
}

export interface Channel {
  id: number;
  type: ChannelType;
  name: string;
  /** Redacted. Never contains a secret, whatever was sent. */
  config: ChannelConfig;
  /** Whether this channel also receives the weekly report. */
  digest: boolean;
  created_at: string;
}

/** The four conditions a rule can watch. The monitor triggers came with uptime and crons. */
export type TriggerKind =
  | "new_issue"
  | "regression"
  | "issue_spike"
  | "error_rate";

/**
 * A rule's condition, stored and sent as the object it is.
 *
 * There is no query language and there is no string to parse: four named
 * conditions with numeric parameters, which is what the builder edits and
 * what the API stores verbatim (ADR 015).
 */
export interface Trigger {
  kind: TriggerKind;
  window_s?: number;
  min_count?: number;
  factor?: number;
  min_events_per_min?: number;
}

export interface AlertRule {
  id: number;
  /** Null means every project. */
  project_id: number | null;
  name: string;
  trigger: Trigger;
  channel_ids: number[];
  silence_seconds: number;
  enabled: boolean;
}

/**
 * Where a queued delivery stands.
 *
 * `failed` and `dead` are the two that must never be shown as one thing: a
 * failed row will be retried on its own and a dead one has spent its attempts
 * and will not. Reading the second as the first is somebody waiting for a
 * message that is never coming.
 */
export type NotificationStatus = "pending" | "sent" | "failed" | "dead";

export interface AlertPayload {
  event: string;
  rule: string;
  project_id: number;
  issue_id?: number;
  title?: string;
  culprit?: string;
  level?: string;
  release?: string;
  environment?: string;
  count?: number;
  baseline?: number;
  url?: string;
  body?: string;
  at: string;
}

export interface Notification {
  id: number;
  rule_id: number;
  channel_id: number;
  subject_key: string;
  status: NotificationStatus;
  attempts: number;
  next_attempt_at: string;
  last_error?: string;
  created_at: string;
  sent_at: string | null;
  payload: AlertPayload;
}

/** One channel's diagnosis: the secret, and the network, kept apart. */
export interface ChannelStatus {
  id: number;
  type: string;
  name: string;
  digest: boolean;
  endpoint?: string;
  secret_ok: boolean;
  reachable: boolean;
  ok: boolean;
  detail: string;
}

export interface ChannelHealth {
  configured: boolean;
  channels: ChannelStatus[];
}

/** The result of asking a rule to deliver a synthetic alert. */
export interface RuleTestResult {
  channel_id: number;
  name: string;
  type: string;
  ok: boolean;
  error?: string;
}

/** When the weekly report goes out. The zone is UTC in this version. */
export interface DigestSchedule {
  weekday: string;
  hour: number;
  timezone: string;
}

/** A background job that is currently running (ADR 014). */
export interface Job {
  name: string;
  interval_seconds: number;
  started_at: string;
  last_run: string | null;
  next_run: string | null;
  last_error: string | null;
  runs: number;
  failures: number;
}

/** The three ways the performance table is ranked. */
export type TransactionSort = "p95" | "count" | "fail";

/**
 * What a reader is actually looking at.
 *
 * It rides along with the table because a sampled number that does not say so
 * misleads: somebody comparing `count` against their load balancer needs to
 * know which of the two was sampled and by how much. The aggregates cover
 * every transaction received — sampling only decides whether the waterfall was
 * kept (ADR 021).
 */
export interface Sampling {
  server_rate: number;
  stored: number;
  received: number;
  effective_rate: number;
}

/** One row of the performance table. */
export interface TransactionSummary {
  transaction: string;
  count: number;
  failed: number;
  fail_rate: number;
  /** How many kept their spans — i.e. whether "show me a slow one" can. */
  sampled: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
  mean_ms: number;
  min_ms: number;
  max_ms: number;
}

export interface TransactionList {
  project: ProjectRef;
  range: RangeBounds;
  sort: TransactionSort;
  total: number;
  total_failed: number;
  sampling: Sampling;
  transactions: TransactionSummary[];
}

/** One bucket of a transaction's history. */
export interface TransactionPoint {
  bucket: string;
  count: number;
  failed: number;
  sampled: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
  fail_rate: number;
}

export interface Span {
  span_id?: string;
  parent_span_id?: string;
  op?: string;
  description?: string;
  status?: string;
  start: string;
  end: string;
  duration_ms: number;
  tags?: Record<string, string>;
  data?: Record<string, unknown>;
}

export interface Trace {
  trace_id: string;
  transaction: string;
  timestamp: string;
  duration_ms: number;
  status?: string;
  op?: string;
  spans: Span[];
}

export interface TransactionSeries {
  project: ProjectRef;
  transaction: string;
  range: RangeBounds;
  resolution: "minute" | "hour";
  total: number;
  /**
   * The whole range merged, not the average of the points: averaging
   * percentiles is exactly the mistake the sketch exists to prevent
   * (ADR 007).
   */
  summary: TransactionSummary;
  series: TransactionPoint[];
  /** The slowest stored waterfalls, so "show me one" costs no round trip. */
  examples: Trace[];
}

/** The four disjoint ends a session can come to (ADR 008). */
export interface SessionCounts {
  started: number;
  errored: number;
  crashed: number;
  abnormal: number;
}

/**
 * The state of the in-memory session window.
 *
 * Shown beside the figure rather than on a diagnostics screen nobody opens: a
 * caveat is only read next to the number it applies to.
 */
export interface SessionWindow {
  in_flight: number;
  capacity: number;
  evicted: number;
  expired: number;
  note: string;
}

export interface HealthPoint extends SessionCounts {
  hour: string;
  /** Null for an hour nothing reported in — not zero, which would read as "everything crashed". */
  crash_free_rate: number | null;
}

export interface ReleaseHealth extends SessionCounts {
  project: ProjectRef;
  range: RangeBounds;
  release: string;
  healthy: number;
  crash_free_rate: number | null;
  series: HealthPoint[];
  window: SessionWindow;
}

export interface ReleaseHealthSummary extends SessionCounts {
  release: string;
  healthy: number;
  crash_free_rate: number | null;
  first_seen: string;
  last_seen: string;
}

export interface ProjectHealth extends SessionCounts {
  project: ProjectRef;
  range: RangeBounds;
  healthy: number;
  crash_free_rate: number | null;
  releases: ReleaseHealthSummary[];
  window: SessionWindow;
}

export const api = {
  setupStatus: () => request<{ needs_setup: boolean }>("GET", "/setup"),
  setup: (username: string, password: string) =>
    request<Admin>("POST", "/setup", { username, password }),
  login: (username: string, password: string) =>
    request<{ status: string }>("POST", "/login", { username, password }),
  logout: () => request<{ status: string }>("POST", "/logout"),
  me: () => request<Admin>("GET", "/me"),

  listProjects: () => request<Project[]>("GET", "/projects"),
  getProject: (id: number) => request<Project>("GET", `/projects/${id}`),
  createProject: (name: string) =>
    request<Project>("POST", "/projects", { name }),
  deleteProject: (id: number) =>
    request<{ status: string }>("DELETE", `/projects/${id}`),
  rotateKey: (id: number) => request<Key>("POST", `/projects/${id}/keys`),
  revokeKey: (publicKey: string) =>
    request<{ status: string }>("DELETE", `/keys/${publicKey}`),

  listIssues: (projectID: number, filter: IssueFilter = {}) =>
    request<IssuePage>("GET", `/projects/${projectID}/issues${issueQuery(filter)}`),
  getProjectConfig: (projectID: number) =>
    request<ProjectConfig>("GET", `/projects/${projectID}/config`),
  setProjectConfig: (projectID: number, changes: ProjectConfigChanges) =>
    request<ProjectConfig>("PUT", `/projects/${projectID}/config`, changes),

  getIssue: (projectID: number, issueID: number) =>
    request<IssueDetail>("GET", `/projects/${projectID}/issues/${issueID}`),
  /**
   * Change an issue's status.
   *
   * `inNextRelease` qualifies "resolved": fixed, and the fix ships next, so
   * events still arriving from the release it is being seen in are expected
   * rather than proof the fix failed (ADR 012).
   */
  setIssueStatus: (
    projectID: number,
    issueID: number,
    status: IssueStatus,
    inNextRelease = false,
  ) =>
    request<{ status: IssueStatus }>(
      "POST",
      `/projects/${projectID}/issues/${issueID}/status`,
      inNextRelease ? { status, in_next_release: true } : { status },
    ),

  projectSeries: (projectID: number, range: DashboardRange) =>
    request<ProjectSeries>(
      "GET",
      `/projects/${projectID}/stats${statsQuery(rangeFor(range))}`,
    ),
  topIssues: (projectID: number, range: DashboardRange, limit = 10) =>
    request<TopIssues>(
      "GET",
      `/projects/${projectID}/stats/top${statsQuery({ ...rangeFor(range), limit })}`,
    ),
  breakdown: (projectID: number, by: "release" | "environment", range: DashboardRange) =>
    request<Breakdown>(
      "GET",
      `/projects/${projectID}/stats/breakdown${statsQuery({ ...rangeFor(range), by, limit: 8 })}`,
    ),
  issueSeries: (projectID: number, issueID: number, window: StatsWindow = "24h") =>
    request<IssueSeries>(
      "GET",
      `/projects/${projectID}/issues/${issueID}/stats${statsQuery({ range: window })}`,
    ),
  /**
   * One request for a whole page of sparklines.
   *
   * Never one call per row: twenty-five rows would be twenty-five round trips
   * for a decoration, and the list would visibly fill in from the top.
   */
  sparklines: (projectID: number, issueIDs: number[], window: StatsWindow = "24h") =>
    request<Sparklines>(
      "GET",
      `/projects/${projectID}/stats/series${statsQuery({ issues: issueIDs.join(","), range: window })}`,
    ),

  listReleases: (projectID: number, limit = 50) =>
    request<{ releases: Release[] }>(
      "GET",
      `/projects/${projectID}/releases${statsQuery({ limit })}`,
    ),
  getRelease: (projectID: number, version: string) =>
    request<ReleaseDetail>(
      "GET",
      `/projects/${projectID}/releases/${encodeURIComponent(version)}`,
    ),

  /**
   * Which change probably caused an issue.
   *
   * Its own request rather than a field on the issue detail, because it reads
   * a release's whole commit set and decodes a stored payload: the stacktrace
   * has to render whether or not the guess succeeds (ADR 019).
   */
  issueSuspects: (projectID: number, issueID: number) =>
    request<SuspectsReport>(
      "GET",
      `/projects/${projectID}/issues/${issueID}/suspects`,
    ),

  /**
   * The whole issue as one markdown document, for pasting into an agent.
   *
   * The panel does not render it — everything in it is already on this screen,
   * laid out better. What the panel is for here is getting it out: an operator
   * who wants to hand this issue to Claude Code should not have to find a
   * token and a curl command first (ADR 006, ADR 022).
   */
  issueBundle: (projectID: number, issueID: number) =>
    requestText(
      `/projects/${projectID}/issues/${issueID}/bundle`,
      "text/markdown",
    ),

  /** The scripts and source maps a project holds, optionally one release's. */
  listArtifacts: (projectID: number, filter: { release?: string } = {}) =>
    request<ArtifactList>(
      "GET",
      `/projects/${projectID}/artifacts${statsQuery({ release: filter.release })}`,
    ),

  /**
   * The performance table: what is slow, what is busy, what is failing.
   *
   * The percentiles are merged out of the stored sketches at read time and
   * were never persisted, which is why asking for a different range gives a
   * different p95 rather than an average of averages (ADR 007, ADR 020).
   */
  listTransactions: (
    projectID: number,
    range: DashboardRange,
    sort: TransactionSort = "p95",
    limit = 25,
  ) =>
    request<TransactionList>(
      "GET",
      `/projects/${projectID}/transactions${statsQuery({ ...rangeFor(range), sort, limit })}`,
    ),
  /**
   * One transaction's history, with example waterfalls riding along.
   *
   * The resolution is asked for rather than derived from the range: minutes
   * answer "what happened during the incident", hours answer "did the fix
   * hold", and deriving it would silently hand a reader the other one.
   */
  transactionSeries: (
    projectID: number,
    transaction: string,
    range: DashboardRange,
    resolution: "minute" | "hour" = "minute",
  ) =>
    request<TransactionSeries>(
      "GET",
      `/projects/${projectID}/transactions/${encodeURIComponent(transaction)}/series${statsQuery(
        { ...rangeFor(range), resolution },
      )}`,
    ),
  getTrace: (projectID: number, traceID: string) =>
    request<Trace>("GET", `/projects/${projectID}/traces/${encodeURIComponent(traceID)}`),

  /** Which release is the bad one — the question the health screen opens with. */
  projectHealth: (projectID: number, range: DashboardRange, limit = 20) =>
    request<ProjectHealth>(
      "GET",
      `/projects/${projectID}/health${statsQuery({ ...rangeFor(range), limit })}`,
    ),
  releaseHealth: (projectID: number, version: string, range: DashboardRange) =>
    request<ReleaseHealth>(
      "GET",
      `/projects/${projectID}/releases/${encodeURIComponent(version)}/health${statsQuery(
        rangeFor(range),
      )}`,
    ),

  listChannels: () =>
    request<{ channels: Channel[] }>("GET", "/alerts/channels"),
  createChannel: (
    type: ChannelType,
    name: string,
    config: ChannelConfig,
    digest: boolean,
  ) =>
    request<Channel>("POST", "/alerts/channels", {
      type,
      name,
      config,
      digest,
    }),
  deleteChannel: (id: number) =>
    request<void>("DELETE", `/alerts/channels/${id}`),
  /**
   * Deliver a sample message now.
   *
   * The one check that proves the credential, which no amount of probing can:
   * a bot token is only proven by using it. It answers 502 with the far end's
   * own words when it fails, so the message is worth showing verbatim.
   */
  testChannel: (id: number) =>
    request<{ ok: boolean; error?: string }>(
      "POST",
      `/alerts/channels/${id}/test`,
    ),

  listRules: () => request<{ rules: AlertRule[] }>("GET", "/alerts/rules"),
  createRule: (rule: {
    project_id: number | null;
    name: string;
    trigger: Trigger;
    channel_ids: number[];
    silence_seconds: number;
  }) => request<AlertRule>("POST", "/alerts/rules", rule),
  deleteRule: (id: number) => request<void>("DELETE", `/alerts/rules/${id}`),
  testRule: (id: number) =>
    request<{ ok: boolean; results: RuleTestResult[] }>(
      "POST",
      `/alerts/rules/${id}/test`,
    ),

  listNotifications: (status?: NotificationStatus, limit = 50) =>
    request<{ notifications: Notification[] }>(
      "GET",
      `/alerts/notifications${statsQuery({ status, limit })}`,
    ),
  retryNotification: (id: number) =>
    request<Notification>("POST", `/alerts/notifications/${id}/retry`),

  /**
   * Probe every channel, without sending anything.
   *
   * It opens a connection per channel and hangs up, so it is asked for rather
   * than run on load: with a channel whose host is unreachable it costs the
   * probe timeout, and a screen that took that long to appear would be a
   * screen nobody opens (ADR 035).
   */
  channelHealth: () => request<ChannelHealth>("GET", "/system/channels"),

  listJobs: () => request<{ jobs: Job[] }>("GET", "/system/jobs"),

  getDigestSchedule: () =>
    request<DigestSchedule>("GET", "/system/settings/digest"),
  setDigestSchedule: (weekday: string, hour: number) =>
    request<DigestSchedule>("PUT", "/system/settings/digest", {
      weekday,
      hour,
    }),

  // Monitors. Two families under one screen because they are one concept
  // watched from two sides (ADR 037): a cron monitor waits to be told, an
  // uptime monitor goes and asks.
  listCronMonitors: (projectID: number) =>
    request<{ monitors: CronMonitor[] }>(
      "GET",
      `/projects/${projectID}/monitors/cron`,
    ),
  createCronMonitor: (
    projectID: number,
    monitor: {
      slug: string;
      schedule: string;
      timezone?: string;
      checkin_margin_s?: number;
      max_runtime_s?: number;
    },
  ) =>
    request<CronMonitor>(
      "POST",
      `/projects/${projectID}/monitors/cron`,
      monitor,
    ),
  deleteCronMonitor: (projectID: number, id: number) =>
    request<void>("DELETE", `/projects/${projectID}/monitors/cron/${id}`),
  listCheckIns: (projectID: number, id: number, limit = 20) =>
    request<{ checkins: CronCheckIn[] }>(
      "GET",
      `/projects/${projectID}/monitors/cron/${id}/checkins?limit=${limit}`,
    ),

  listUptimeMonitors: (projectID: number) =>
    request<{ monitors: UptimeMonitor[] }>(
      "GET",
      `/projects/${projectID}/monitors/uptime`,
    ),
  createUptimeMonitor: (
    projectID: number,
    monitor: {
      name: string;
      url: string;
      method?: string;
      interval_s?: number;
      expected_body_substring?: string;
      public?: boolean;
    },
  ) =>
    request<UptimeMonitor>(
      "POST",
      `/projects/${projectID}/monitors/uptime`,
      monitor,
    ),
  deleteUptimeMonitor: (id: number) =>
    request<void>("DELETE", `/monitors/uptime/${id}`),
  setUptimeMonitorEnabled: (id: number, enabled: boolean) =>
    request<UptimeMonitor>("POST", `/monitors/uptime/${id}/enabled`, {
      enabled,
    }),
  listUptimeResults: (id: number, limit = 20) =>
    request<{ results: UptimeResult[] }>(
      "GET",
      `/monitors/uptime/${id}/results?limit=${limit}`,
    ),
  uptimeDaily: (id: number, days = 90) =>
    request<{ days: UptimeDay[] }>("GET", `/monitors/uptime/${id}/daily?days=${days}`),

  getStatusPageSettings: () =>
    request<StatusPageSettings>("GET", "/system/settings/status-page"),
  setStatusPageSettings: (title: string, description: string) =>
    request<StatusPageSettings>("PUT", "/system/settings/status-page", {
      title,
      description,
    }),
};
