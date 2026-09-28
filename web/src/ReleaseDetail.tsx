// One release, and the two questions a deploy raises.
//
// Did I break something new, and did I bring something back. Both numbers come
// from indexed columns on `issues` and from the hourly buckets (ADR 010/012),
// so this screen stays answerable long after the events it counted are gone.

import { useEffect, useState } from "react";
import {
  ApiError,
  api,
  type ArtifactList,
  type ReleaseCommit,
  type ReleaseDeploy,
  type ReleaseDetail as Detail,
  type ReleaseHealth,
} from "./api";
import { Sparkline } from "./charts";
import { Link, issuesPath, projectsPath, releasesPath } from "./routes";
import { Ago, Header, Notice, ProjectNav } from "./ui";

export function ReleaseDetail({
  projectID,
  version,
  onSignedOut,
}: {
  projectID: number;
  version: string;
  onSignedOut: () => void;
}) {
  const [detail, setDetail] = useState<Detail | null>(null);
  const [health, setHealth] = useState<ReleaseHealth | null>(null);
  const [artifacts, setArtifacts] = useState<ArtifactList | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let current = true;
    setLoading(true);
    (async () => {
      try {
        const loaded = await api.getRelease(projectID, version);
        if (!current) return;
        setDetail(loaded);
        setError("");
      } catch (err) {
        if (!current) return;
        if (err instanceof ApiError && err.isUnauthorized) {
          onSignedOut();
          return;
        }
        setError(err instanceof Error ? err.message : "could not load the release");
      } finally {
        if (current) setLoading(false);
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, version, onSignedOut]);

  // Release health, loaded separately and allowed to fail in silence.
  //
  // Sessions are opt-in and most installations never send one, so a release
  // page that refused to render without a crash-free rate would be a page
  // broken by a feature nobody switched on. The counters come from four
  // numbers per hour and never from a row per session (ADR 008).
  useEffect(() => {
    let current = true;
    setHealth(null);
    (async () => {
      try {
        const loaded = await api.releaseHealth(projectID, version, "7d");
        if (current) setHealth(loaded);
      } catch {
        // No sessions, no section.
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, version]);

  // The scripts and maps uploaded for this release, loaded separately: if the
  // artefact endpoint fails, the page somebody opened to see what a deploy
  // broke still shows what it broke (ADR 018).
  useEffect(() => {
    let current = true;
    setArtifacts(null);
    (async () => {
      try {
        const loaded = await api.listArtifacts(projectID, { release: version });
        if (current) setArtifacts(loaded);
      } catch {
        // A missing list is a missing list.
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, version]);

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <Header onSignedOut={onSignedOut}>
        <Link to={projectsPath} className="font-normal text-muted hover:text-accent">
          Projects
        </Link>
        <span className="px-2 text-muted">/</span>
        <Link
          to={releasesPath(projectID)}
          className="font-normal text-muted hover:text-accent"
        >
          Releases
        </Link>
        <span className="px-2 text-muted">/</span>
        <span className="font-mono">{version}</span>
      </Header>

      <ProjectNav projectID={projectID} current="releases" />

      <Notice>{error}</Notice>

      {loading ? (
        <p className="text-sm text-muted">loading…</p>
      ) : !detail ? null : (
        <>
          <section className="space-y-3">
            <dl className="grid grid-cols-3 gap-3">
              <Figure
                label="New issues"
                value={detail.new_issues}
                note="first seen in this release"
                to={issuesPath(projectID, { release: detail.version })}
              />
              <Figure
                label="Regressed"
                value={detail.regressed_issues}
                note="came back in this release"
              />
              <Figure
                label="Events"
                value={detail.events}
                note="from the hourly buckets"
              />
            </dl>

            <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted">
              <span>
                registered <Ago at={detail.created_at} />
              </span>
              {detail.first_event_at && (
                <span>
                  first event <Ago at={detail.first_event_at} />
                </span>
              )}
              {detail.last_event_at && (
                <span>
                  last event <Ago at={detail.last_event_at} />
                </span>
              )}
              <span>
                {detail.date_released ? (
                  <>
                    released <Ago at={detail.date_released} />
                  </>
                ) : (
                  "not finalised"
                )}
              </span>
            </p>
          </section>

          <Health health={health} />
          <Deploys deploys={detail.deploys} />
          <Commits commits={detail.commits} />
          <Artifacts list={artifacts} version={detail.version} />
        </>
      )}
    </div>
  );
}

function Figure({
  label,
  value,
  note,
  to,
  suffix = "",
}: {
  label: string;
  value: number;
  note: string;
  to?: string;
  suffix?: string;
}) {
  const body = (
    <>
      <dt className="text-xs text-muted">{label}</dt>
      <dd className="mt-0.5 text-2xl font-semibold tabular-nums">
        {value.toLocaleString()}
        {suffix}
      </dd>
      <p className="mt-0.5 text-xs text-muted">{note}</p>
    </>
  );
  if (!to) {
    return <div className="rounded-md border border-line p-3">{body}</div>;
  }
  return (
    <Link
      to={to}
      className="block rounded-md border border-line p-3 transition hover:bg-line/30"
    >
      {body}
    </Link>
  );
}

/**
 * Health is the crash-free rate, and the caveat that comes with it.
 *
 * The caveat is not a footnote. Sessions in flight live in a bounded window in
 * memory, a restart loses them, and the figure for the current hour can move
 * afterwards. Saying that beside the number is the difference between a reader
 * trusting the product and a reader discovering it on their own during an
 * incident (ADR 008).
 */
function Health({ health }: { health: ReleaseHealth | null }) {
  if (!health || health.started === 0) return null;

  const crashFree = health.crash_free_rate;
  const crashes = health.series.map((point) => point.crashed);
  const worst = health.series.reduce<{ hour: string; rate: number } | null>(
    (found, point) => {
      if (point.crash_free_rate === null) return found;
      if (found && found.rate <= point.crash_free_rate) return found;
      return { hour: point.hour, rate: point.crash_free_rate };
    },
    null,
  );

  return (
    <section className="space-y-3 border-t border-line pt-4">
      <h2 className="text-sm font-medium">Release health</h2>

      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Figure
          label="Crash-free"
          value={crashFree === null ? 0 : Math.round(crashFree * 10000) / 100}
          note={crashFree === null ? "no sessions yet" : "of sessions ended cleanly"}
          suffix="%"
        />
        <Figure label="Sessions" value={health.started} note="started in this range" />
        <Figure label="Crashed" value={health.crashed} note="ended in a crash" />
        <Figure
          label="Errored"
          value={health.errored}
          note="saw an error but survived"
        />
      </dl>

      {crashes.some((count) => count > 0) && (
        <div className="flex items-baseline gap-3">
          <Sparkline counts={crashes} label="Crashes per hour" className="h-6 w-32" />
          <p className="text-xs text-muted">
            crashes per hour
            {worst &&
              `, worst hour ${(worst.rate * 100).toFixed(2)}% crash-free`}
          </p>
        </div>
      )}

      <p className="text-xs text-muted">{health.window.note}</p>
      {health.window.evicted > 0 && (
        <p className="text-xs text-muted">
          {health.window.evicted.toLocaleString()} sessions were settled early
          because the window was full ({health.window.capacity.toLocaleString()}{" "}
          entries). Precision was traded for stability, which is the direction
          this product chooses on purpose — the counts are floors, never
          inventions.
        </p>
      )}
    </section>
  );
}

function Deploys({ deploys }: { deploys: ReleaseDeploy[] }) {
  if (deploys.length === 0) return null;
  return (
    <section className="space-y-2 border-t border-line pt-4">
      <h2 className="text-sm font-medium">Deploys</h2>
      <ul className="space-y-1">
        {deploys.map((deploy) => (
          <li key={deploy.id} className="flex items-baseline gap-3 text-xs">
            <span className="w-24 shrink-0 truncate font-medium">
              {deploy.environment}
            </span>
            <span className="min-w-0 flex-1 truncate text-muted">
              {deploy.url ? (
                // The URL a deploy tool recorded is the only thing on this
                // screen that points outside the installation, so it opens in
                // a new tab and carries the rel that keeps the opener private.
                <a
                  href={deploy.url}
                  target="_blank"
                  rel="noreferrer noopener"
                  className="hover:text-accent"
                >
                  {deploy.name || deploy.url}
                </a>
              ) : (
                deploy.name || "—"
              )}
            </span>
            <span className="shrink-0 text-muted">
              {deploy.finished_at ? (
                <Ago at={deploy.finished_at} />
              ) : deploy.started_at ? (
                <>
                  started <Ago at={deploy.started_at} />
                </>
              ) : (
                "—"
              )}
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}

function Commits({ commits }: { commits: ReleaseCommit[] }) {
  return (
    <section className="space-y-2 border-t border-line pt-4">
      <h2 className="text-sm font-medium">
        Commits
        {commits.length > 0 && (
          <span className="ml-2 text-xs text-muted"> {commits.length}</span>
        )}
      </h2>
      {commits.length === 0 ? (
        <p className="py-2 text-sm text-muted">
          No commits associated. A deploy tool sends them —{" "}
          <code className="font-mono text-xs">
            trapline releases commits
          </code>{" "}
          or <code className="font-mono text-xs">sentry-cli set-commits</code> —
          and the file paths they carry are what will let this say which change
          is the suspect.
        </p>
      ) : (
        <ul className="space-y-2">
          {commits.map((commit) => (
            <li key={commit.id} className="text-xs">
              <div className="flex items-baseline gap-2">
                {/* The short sha, because that is what anyone types into
                    `git show`. The full one is in the title for copying. */}
                <code className="shrink-0 font-mono text-muted" title={commit.id}>
                  {commit.id.slice(0, 8)}
                </code>
                <span className="min-w-0 truncate">
                  {firstLine(commit.message) || "no message"}
                </span>
              </div>
              <p className="mt-0.5 flex flex-wrap gap-x-2 text-muted">
                {commit.author_name && <span>{commit.author_name}</span>}
                {commit.timestamp && <Ago at={commit.timestamp} />}
                {commit.patch_set && commit.patch_set.length > 0 && (
                  <span>
                    {commit.patch_set.length}{" "}
                    {commit.patch_set.length === 1 ? "file" : "files"}
                  </span>
                )}
              </p>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/**
 * Artifacts is what was uploaded for this release: the scripts and the maps
 * that turn its minified stacktraces back into source.
 *
 * On the release page rather than a page of its own, because the question is
 * always about one deploy — "did this build's maps actually arrive?" — and
 * the answer is worth nothing a click away from the errors it explains
 * (ADR 018).
 */
function Artifacts({
  list,
  version,
}: {
  list: ArtifactList | null;
  version: string;
}) {
  // Null while it is loading, and while it is loading there is nothing
  // honest to say: an empty state shown before the answer arrives reads as
  // "nothing was uploaded", which is the one thing it must not say wrongly.
  if (!list) return null;

  return (
    <section className="space-y-2 border-t border-line pt-4">
      <div className="flex items-baseline justify-between gap-3">
        <h2 className="text-sm font-medium">
          Artifacts
          {list.artifacts.length > 0 && (
            <span className="ml-2 text-xs text-muted">
              {" "}
              {list.artifacts.length}
            </span>
          )}
        </h2>
        {list.budget_bytes > 0 && (
          <span className="shrink-0 text-xs text-muted">
            {formatBytes(list.used_bytes)} of {formatBytes(list.budget_bytes)}{" "}
            used by this project
          </span>
        )}
      </div>

      {list.artifacts.length === 0 ? (
        <p className="py-2 text-sm text-muted">
          Nothing uploaded for this release. Without its source maps, a
          stacktrace from this build names the minified bundle and nothing
          else —{" "}
          <code className="font-mono text-xs">
            trapline artifacts upload -project &lt;id&gt; -release {version}{" "}
            &lt;dir&gt;
          </code>{" "}
          or <code className="font-mono text-xs">sentry-cli sourcemaps upload</code>.
        </p>
      ) : (
        <ul className="space-y-1">
          {list.artifacts.map((artifact) => (
            <li key={artifact.id} className="flex items-baseline gap-3 text-xs">
              <span className="w-24 shrink-0 truncate text-muted">
                {artifact.kind === "source_map" ? "source map" : "script"}
                {artifact.dist && ` · ${artifact.dist}`}
              </span>
              <span className="min-w-0 flex-1 truncate font-mono" title={artifact.name}>
                {artifact.name}
              </span>
              {/* The debug id is what joins a script to its map without a
                  release, so it is the field somebody checks when an upload
                  "arrived" but nothing resolves (ADR 018). */}
              <span
                className="hidden shrink-0 font-mono text-muted sm:inline"
                title={artifact.debug_id}
              >
                {artifact.debug_id ? artifact.debug_id.slice(0, 8) : "no debug id"}
              </span>
              <span className="shrink-0 text-muted tabular-nums">
                {formatBytes(artifact.size)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** Sizes are read at a glance, so they are rounded rather than exact. */
function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`;
}

/** A commit message is a subject and then a body; a list wants the subject. */
function firstLine(message?: string): string {
  return (message ?? "").split("\n", 1)[0] ?? "";
}
