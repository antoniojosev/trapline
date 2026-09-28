// The screen somebody opens to ask "how bad is it, and since when".
//
// Every number here comes from the hourly aggregates written during ingest
// (ADR 010), never from the events themselves — which is what makes "is this
// worse than last month" answerable after retention has deleted the payloads
// that month was counted from.

import { useEffect, useState } from "react";
import {
  ApiError,
  api,
  type Breakdown,
  type DashboardRange,
  type ProjectSeries,
  type Sparklines,
  type TopIssues,
} from "./api";
import { HourlyBars, RankedBars, Sparkline } from "./charts";
import { Link, issuePath, issuesPath, navigate, projectsPath } from "./routes";
import { Ago, Header, LevelTag, Notice, ProjectNav, StatusTag } from "./ui";

// Three windows and no more. A free-form date picker is the sort of control
// that gets built before anyone has needed it: the questions this screen
// answers are "right now", "this week" and "this month", and the API takes two
// arbitrary instants for the day a fourth question turns up.
const RANGES: { key: DashboardRange; label: string }[] = [
  { key: "24h", label: "24 hours" },
  { key: "7d", label: "7 days" },
  { key: "30d", label: "30 days" },
];

interface Loaded {
  series: ProjectSeries;
  top: TopIssues;
  releases: Breakdown;
  environments: Breakdown;
}

export function Dashboard({
  projectID,
  onSignedOut,
}: {
  projectID: number;
  onSignedOut: () => void;
}) {
  const [range, setRange] = useState<DashboardRange>("24h");
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [sparklines, setSparklines] = useState<Sparklines | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let current = true;
    setLoading(true);
    (async () => {
      try {
        // In parallel, because they are four independent reads of the same
        // buckets and doing them in sequence would make the screen appear in
        // four steps for no reason.
        const [series, top, releases, environments] = await Promise.all([
          api.projectSeries(projectID, range),
          api.topIssues(projectID, range),
          api.breakdown(projectID, "release", range),
          api.breakdown(projectID, "environment", range),
        ]);
        if (!current) return;
        setLoaded({ series, top, releases, environments });
        setError("");
      } catch (err) {
        if (!current) return;
        if (err instanceof ApiError && err.isUnauthorized) {
          onSignedOut();
          return;
        }
        setError(err instanceof Error ? err.message : "could not load the dashboard");
      } finally {
        if (current) setLoading(false);
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, range, onSignedOut]);

  // The sparklines are a second request on purpose: they need the ids the top
  // list returns, and they are a decoration. Failing to draw them must not
  // cost the screen the numbers it exists for, so this has no error branch —
  // it simply leaves them out.
  const topIDs = loaded?.top.issues.map((issue) => issue.id) ?? [];
  const topKey = topIDs.join(",");
  useEffect(() => {
    let current = true;
    setSparklines(null);
    if (topKey === "") return;
    (async () => {
      try {
        const drawn = await api.sparklines(
          projectID,
          topKey.split(",").map(Number),
        );
        if (current) setSparklines(drawn);
      } catch {
        // See above: a missing sparkline is a missing decoration.
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, topKey]);

  const countsFor = (issueID: number): number[] =>
    sparklines?.issues.find((entry) => entry.issue_id === issueID)?.counts ?? [];

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <Header onSignedOut={onSignedOut}>
        <Link to={projectsPath} className="font-normal text-muted hover:text-accent">
          Projects
        </Link>
        <span className="px-2 text-muted">/</span>
        {loaded?.series.project.name ?? `#${projectID}`}
      </Header>

      <ProjectNav projectID={projectID} current="dashboard" />

      <div className="flex flex-wrap items-baseline justify-between gap-3">
        {/* Buttons, not links. The range is state, not an address: it
            changes what one screen is showing, and putting it in the URL
            would make the back button walk the reader through three chart
            widths before it left the page. */}
        <div className="flex gap-1" role="group" aria-label="Range">
          {RANGES.map((option) => {
            const active = option.key === range;
            return (
              <button
                key={option.key}
                type="button"
                aria-pressed={active}
                onClick={() => setRange(option.key)}
                className={`rounded-md px-2.5 py-1 text-sm transition ${
                  active ? "bg-line/60 font-medium" : "text-muted hover:text-ink"
                }`}
              >
                {option.label}
              </button>
            );
          })}
        </div>
        {loaded && (
          <p className="text-sm text-muted tabular-nums">
            {loaded.series.total.toLocaleString()}{" "}
            {loaded.series.total === 1 ? "event" : "events"} over{" "}
            {loaded.series.range.hours} hours
          </p>
        )}
      </div>

      <Notice>{error}</Notice>

      {loading && !loaded ? (
        <p className="text-sm text-muted">loading…</p>
      ) : !loaded ? null : (
        <>
          <section className="space-y-2">
            <h2 className="text-sm font-medium">Events per hour</h2>
            <HourlyBars
              points={loaded.series.series}
              levels={loaded.series.by_level}
            />
          </section>

          <section className="space-y-2 border-t border-line pt-4">
            <div className="flex items-baseline justify-between gap-3">
              <h2 className="text-sm font-medium">
                Loudest issues
                <span className="ml-2 text-xs text-muted">
                  {" "}
                  in this range, not all time
                </span>
              </h2>
              <Link
                to={issuesPath(projectID)}
                className="text-xs text-muted hover:text-accent"
              >
                all issues
              </Link>
            </div>
            {loaded.top.issues.length === 0 ? (
              <p className="py-4 text-sm text-muted">
                Nothing was recorded in this range.
              </p>
            ) : (
              <ul>
                {loaded.top.issues.map((issue) => (
                  <li key={issue.id} className="border-b border-line">
                    <Link
                      to={issuePath(projectID, issue.id)}
                      className="-mx-2 flex items-center justify-between gap-4 rounded px-2 py-2.5 hover:bg-line/30"
                    >
                      <div className="min-w-0">
                        <div className="flex items-baseline gap-2">
                          <LevelTag level={issue.level} />
                          <StatusTag status={issue.status} />
                          <span className="truncate font-medium">{issue.title}</span>
                        </div>
                        <p className="mt-0.5 truncate font-mono text-xs text-muted">
                          {issue.culprit || "no location recorded"}
                        </p>
                      </div>
                      <div className="flex shrink-0 items-center gap-3">
                        <Sparkline
                          counts={countsFor(issue.id)}
                          label={issue.title}
                        />
                        <div className="w-16 text-right text-xs text-muted">
                          <div className="tabular-nums">
                            {issue.count.toLocaleString()}×
                          </div>
                          <div className="mt-0.5">
                            <Ago at={issue.last_seen} />
                          </div>
                        </div>
                      </div>
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </section>

          <div className="grid gap-6 border-t border-line pt-4 sm:grid-cols-2">
            <section className="space-y-2">
              <h2 className="text-sm font-medium">
                By release
                <span className="ml-2 text-xs text-muted"> which build</span>
              </h2>
              <RankedBars
                items={loaded.releases.values}
                empty="No release reported anything in this range."
                onSelect={(value) =>
                  navigate(issuesPath(projectID, { release: value }))
                }
              />
            </section>
            <section className="space-y-2">
              <h2 className="text-sm font-medium">
                By environment
                <span className="ml-2 text-xs text-muted"> where</span>
              </h2>
              <RankedBars
                items={loaded.environments.values}
                empty="No environment reported anything in this range."
                onSelect={(value) =>
                  navigate(issuesPath(projectID, { environment: value }))
                }
              />
            </section>
          </div>
        </>
      )}
    </div>
  );
}
