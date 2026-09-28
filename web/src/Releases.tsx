// What has shipped, newest first.
//
// The list is deliberately thin — a version, when it was first and last seen,
// how many commits it carries. The numbers that cost a query per release, and
// that are the reason anyone opens this screen at all, live one click away on
// the detail.
//
// With one exception, added when release health arrived: the crash-free rate.
// It is here because "which of these is the bad one" is the question somebody
// opens a release list with, and it costs one request for the whole page — the
// project-wide health endpoint returns every release that reported sessions in
// the range, so the alternative would have been a request per row to answer
// the same question worse (ADR 008).

import { useEffect, useState } from "react";
import { ApiError, api, type ProjectHealth, type Release } from "./api";
import { Link, projectsPath, releasePath } from "./routes";
import { Ago, Header, Notice, ProjectNav } from "./ui";

export function Releases({
  projectID,
  onSignedOut,
}: {
  projectID: number;
  onSignedOut: () => void;
}) {
  const [releases, setReleases] = useState<Release[] | null>(null);
  const [health, setHealth] = useState<ProjectHealth | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let current = true;
    setLoading(true);
    (async () => {
      try {
        const loaded = await api.listReleases(projectID);
        if (!current) return;
        setReleases(loaded.releases);
        setError("");
      } catch (err) {
        if (!current) return;
        if (err instanceof ApiError && err.isUnauthorized) {
          onSignedOut();
          return;
        }
        setError(err instanceof Error ? err.message : "could not load releases");
      } finally {
        if (current) setLoading(false);
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, onSignedOut]);

  // A second request, and one that is allowed to fail in silence. Sessions are
  // opt-in: most installations never send one, and a release list that refused
  // to render because a crash-free rate was unavailable would be a screen
  // broken by a feature nobody switched on.
  useEffect(() => {
    let current = true;
    setHealth(null);
    (async () => {
      try {
        const loaded = await api.projectHealth(projectID, "30d");
        if (current) setHealth(loaded);
      } catch {
        // No sessions, no column.
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID]);

  const crashFree = (version: string): number | null =>
    health?.releases.find((entry) => entry.release === version)
      ?.crash_free_rate ?? null;

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <Header onSignedOut={onSignedOut}>
        <Link to={projectsPath} className="font-normal text-muted hover:text-accent">
          Projects
        </Link>
        <span className="px-2 text-muted">/</span>
        Releases
      </Header>

      <ProjectNav projectID={projectID} current="releases" />

      <Notice>{error}</Notice>

      {loading ? (
        <p className="text-sm text-muted">loading…</p>
      ) : !releases || releases.length === 0 ? (
        <p className="text-sm text-muted">
          No releases yet. One appears the moment an event arrives carrying a{" "}
          <code className="font-mono">release</code>, so this fills in on its
          own — a deploy tool registering them up front is the exception, not
          the requirement.
        </p>
      ) : (
        <ul>
          {releases.map((release) => (
            <li key={release.version} className="border-b border-line">
              <Link
                to={releasePath(projectID, release.version)}
                className="-mx-2 flex items-baseline justify-between gap-4 rounded px-2 py-3 hover:bg-line/30"
              >
                <div className="min-w-0">
                  <div className="flex items-baseline gap-2">
                    <span className="truncate font-mono font-medium">
                      {release.version}
                    </span>
                    {/* "Not finalised" is not a warning. Most installations
                        never run a deploy tool, so the honest label for a
                        release nobody declared shipped is that nobody
                        declared it, not that something is wrong. */}
                    {release.date_released ? (
                      <span className="shrink-0 rounded border border-line px-1.5 py-0.5 text-xs text-muted">
                        released
                      </span>
                    ) : null}
                  </div>
                  <p className="mt-1 text-xs text-muted">
                    {release.first_event_at ? (
                      <>
                        first event <Ago at={release.first_event_at} />
                      </>
                    ) : (
                      "no events yet"
                    )}
                    {release.commit_count > 0 && (
                      <>
                        {" · "}
                        {release.commit_count.toLocaleString()}{" "}
                        {release.commit_count === 1 ? "commit" : "commits"}
                      </>
                    )}
                  </p>
                </div>
                <div className="shrink-0 space-y-0.5 text-right text-xs text-muted">
                  <div>
                    {release.last_event_at ? (
                      <Ago at={release.last_event_at} />
                    ) : (
                      "—"
                    )}
                  </div>
                  <CrashFree rate={crashFree(release.version)} />
                </div>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * CrashFree renders the rate, or nothing at all.
 *
 * Nothing at all, rather than a dash or a zero: a release that reported no
 * sessions has not told us it is healthy and has not told us it is broken, and
 * both of those placeholders read as one of the two. The word "crash-free"
 * travels with the number because "99.2%" on its own is a percentage of
 * something the reader has to guess.
 */
function CrashFree({ rate }: { rate: number | null }) {
  if (rate === null) return null;
  const percent = rate * 100;
  // Three decimals below 100 and none at it: the difference between 99.95%
  // and 99.99% is the whole conversation on a busy service, and rounding it
  // to "100%" is the one rendering that could hide an outage.
  const text = percent >= 99.9 && percent < 100
    ? `${percent.toFixed(2)}%`
    : `${percent.toFixed(percent >= 99 ? 1 : 0)}%`;
  return (
    <div className={percent < 99 ? "text-red-600" : ""}>
      <span className="tabular-nums">{text}</span> crash-free
    </div>
  );
}
