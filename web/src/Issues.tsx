import { useEffect, useMemo, useState } from "react";
import {
  ApiError,
  api,
  type Issue,
  type IssuePage,
  type IssueStatus,
  type Sparklines,
} from "./api";
import { Sparkline } from "./charts";
import { useLiveIssues } from "./live";
import {
  Link,
  issuePath,
  projectsPath,
  useIssueFilters,
  type IssueFilters,
} from "./routes";
import {
  Ago,
  Button,
  Header,
  Input,
  LevelTag,
  Notice,
  ProjectNav,
  StatusTag,
} from "./ui";

// Unresolved first, because this is the screen someone opens when something is
// broken and the question is what is broken now, not what has ever been.
const FILTERS: { label: string; status?: IssueStatus }[] = [
  { label: "Unresolved", status: "unresolved" },
  { label: "Resolved", status: "resolved" },
  { label: "Ignored", status: "ignored" },
  { label: "All" },
];

export function Issues({
  projectID,
  onSignedOut,
}: {
  projectID: number;
  onSignedOut: () => void;
}) {
  const [page, setPage] = useState<IssuePage | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const issues = page?.issues ?? [];
  const [status, setStatus] = useState<IssueStatus | undefined>("unresolved");
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState("");
  const [error, setError] = useState("");
  // A search the index cannot run is not an error about the panel; it is the
  // server telling the reader something about what they typed. Kept apart from
  // `error` so it can be said next to the box rather than above the list.
  const [searchError, setSearchError] = useState("");
  const [loading, setLoading] = useState(true);
  const [reloads, setReloads] = useState(0);
  const [sparklines, setSparklines] = useState<Sparklines | null>(null);

  // The tag filters live in the URL, because they are what somebody shares:
  // "it is only Safari" is a link, and the link has to reopen the list already
  // narrowed. The status filter above does not, because it is the reader's
  // current question rather than a fact about the issues.
  const [filters, setFilters] = useIssueFilters();
  const [applied, setApplied] = useState<IssueFilters>(filters);
  const { environment, release, tag } = applied;

  function setFilter(name: keyof IssueFilters, value: string) {
    setFilters({ ...filters, [name]: value });
  }

  // Typing a title should not be one request per keystroke against a server
  // that is also swallowing an incident's worth of events. The same applies to
  // the three filter boxes, so they share the delay.
  useEffect(() => {
    const timer = setTimeout(() => setSearch(query.trim()), 200);
    return () => clearTimeout(timer);
  }, [query]);

  useEffect(() => {
    const timer = setTimeout(() => setApplied(filters), 200);
    return () => clearTimeout(timer);
  }, [filters]);

  useEffect(() => {
    let current = true;
    setLoading(true);
    (async () => {
      try {
        const loaded = await api.listIssues(projectID, {
          status,
          q: search,
          environment,
          release,
          tag,
        });
        if (!current) return;
        setPage(loaded);
        setError("");
        setSearchError("");
      } catch (err) {
        if (!current) return;
        if (err instanceof ApiError && err.isUnauthorized) {
          onSignedOut();
          return;
        }
        // The distinction that matters: a query the index refused never ran,
        // and answering it with an empty list would send somebody looking for
        // a bug in their own data. The server says which term was too short
        // and why (ADR 011), so that sentence is shown as it arrived.
        if (err instanceof ApiError && err.isUnaskable && search !== "") {
          setSearchError(err.message);
          setPage(null);
          setError("");
          return;
        }
        setError(err instanceof Error ? err.message : "could not load issues");
      } finally {
        if (current) setLoading(false);
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, status, search, environment, release, tag, reloads, onSignedOut]);

  // One request for every row's sparkline rather than one per row. Keyed by
  // the ids on screen so paging extends it instead of redrawing it.
  const shownIDs = issues.map((issue) => issue.id).join(",");
  useEffect(() => {
    let current = true;
    setSparklines(null);
    if (shownIDs === "") return;
    (async () => {
      try {
        const drawn = await api.sparklines(
          projectID,
          shownIDs.split(",").map(Number),
        );
        if (current) setSparklines(drawn);
      } catch {
        // A sparkline is a decoration. Losing it must not cost the list.
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, shownIDs]);

  const known = useMemo(
    () => new Set(issues.map((issue) => issue.id)),
    // The set is rebuilt when the rows change, which is exactly what the live
    // feed needs to decide whether an arrival is already on screen.
    [issues],
  );
  const live = useLiveIssues(projectID, known);

  // Paging appends rather than replaces: someone reading down a list and
  // asking for more expects the list to grow, not to jump.
  async function loadMore() {
    if (!page?.next_cursor || loadingMore) return;
    setLoadingMore(true);
    try {
      const next = await api.listIssues(projectID, {
        status,
        q: search,
        environment,
        release,
        tag,
        cursor: page.next_cursor,
      });
      setPage({ ...next, issues: [...page.issues, ...next.issues] });
    } catch (err) {
      setError(err instanceof Error ? err.message : "could not load more");
    } finally {
      setLoadingMore(false);
    }
  }

  const countsFor = (issueID: number): number[] =>
    sparklines?.issues.find((entry) => entry.issue_id === issueID)?.counts ?? [];

  const activeFilters = (
    [
      ["environment", `environment: ${environment}`],
      ["release", `release: ${release}`],
      ["tag", tag],
    ] as [keyof IssueFilters, string][]
  ).filter(([name]) => applied[name] !== "");

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <Header onSignedOut={onSignedOut}>
        <Link
          to={projectsPath}
          className="font-normal text-muted hover:text-accent"
        >
          Projects
        </Link>
        <span className="px-2 text-muted">/</span>
        {/* The name rides along with the page of issues, so the heading and
            the rows under it can never disagree about which project this is
            and there is no second request to fail on its own. */}
        {page?.project.name ?? `#${projectID}`}
      </Header>

      <ProjectNav projectID={projectID} current="issues" />

      <div className="flex flex-wrap items-center gap-3">
        <div className="flex gap-1">
          {FILTERS.map((filter) => {
            const active = filter.status === status;
            return (
              <button
                key={filter.label}
                type="button"
                aria-pressed={active}
                onClick={() => setStatus(filter.status)}
                className={`rounded-md px-2.5 py-1 text-sm transition ${
                  active
                    ? "bg-line/60 font-medium"
                    : "text-muted hover:text-ink"
                }`}
              >
                {filter.label}
                {/* The count comes from the server and covers the project, not
                    the current filter, so a number never changes meaning
                    depending on which button is already pressed. */}
                {filter.status && page ? (
                  <span className="ml-1.5 text-xs text-muted">
                    {page.counts[filter.status] ?? 0}
                  </span>
                ) : null}
              </button>
            );
          })}
        </div>
        {/* Environment and release are ordinary tags promoted on ingest, but
            they get their own controls because "what is broken in production"
            and "did that deploy do this" are the first two things anyone asks
            during an incident. */}
        <div className="w-full sm:w-36">
          <Input
            type="search"
            value={filters.environment}
            onChange={(e) => setFilter("environment", e.target.value)}
            placeholder="environment"
            aria-label="Filter by environment"
          />
        </div>
        <div className="w-full sm:w-40">
          <Input
            type="search"
            value={filters.release}
            onChange={(e) => setFilter("release", e.target.value)}
            placeholder="release"
            aria-label="Filter by release"
          />
        </div>
        <div className="ml-auto w-full sm:w-56">
          <Input
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search titles"
            aria-label="Search issues by title"
            aria-invalid={searchError !== ""}
            aria-describedby={searchError ? "search-error" : undefined}
          />
        </div>
      </div>

      {activeFilters.length > 0 && (
        <p className="flex flex-wrap items-center gap-2 text-xs">
          <span className="text-muted">Filtered by</span>
          {activeFilters.map(([name, label]) => (
            <button
              key={name}
              type="button"
              onClick={() => setFilter(name, "")}
              className="rounded border border-line px-1.5 py-0.5 font-mono hover:bg-line/40"
              aria-label={`Remove the ${label} filter`}
            >
              {label} <span aria-hidden="true">×</span>
            </button>
          ))}
        </p>
      )}

      {searchError && (
        <p
          id="search-error"
          role="alert"
          className="rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm"
        >
          {searchError}
        </p>
      )}

      <Notice>{error}</Notice>

      {/* The live feed does not touch the list. Reordering rows under somebody
          reading them — during an incident, which is when this matters — moves
          the line they were about to click. So arrivals wait behind a button
          the reader presses when they are ready. */}
      {live.arrived.length > 0 && (
        <button
          type="button"
          onClick={() => setReloads((count) => count + 1)}
          className="w-full rounded-md border border-accent/50 bg-accent/10 px-3 py-2 text-sm font-medium transition hover:bg-accent/20"
        >
          {live.arrived.length}{" "}
          {live.arrived.length === 1 ? "new issue" : "new issues"} — show{" "}
          {live.arrived.length === 1 ? "it" : "them"}
        </button>
      )}

      {loading ? (
        <p className="text-sm text-muted">loading…</p>
      ) : searchError ? null : issues.length === 0 ? (
        <p className="text-sm text-muted">
          {search
            ? `No issue title contains “${search}”.`
            : activeFilters.length > 0
              ? "No issue matches these filters."
              : status === "unresolved"
                ? "Nothing unresolved. Either it is quiet or someone has been triaging."
                : "No issues here yet."}
        </p>
      ) : (
        <>
          <ul>
            {issues.map((issue: Issue) => (
              <li key={issue.id} className="border-b border-line">
                <Link
                  to={issuePath(projectID, issue.id)}
                  className="-mx-2 flex items-center justify-between gap-4 rounded px-2 py-3 hover:bg-line/30"
                >
                  <div className="min-w-0">
                    <div className="flex items-baseline gap-2">
                      <LevelTag level={issue.level} />
                      <StatusTag status={issue.status} />
                      <span className="truncate font-medium">
                        {issue.title}
                      </span>
                    </div>
                    <p className="mt-1 truncate font-mono text-xs text-muted">
                      {issue.culprit || "no location recorded"}
                    </p>
                  </div>
                  <div className="flex shrink-0 items-center gap-3">
                    <Sparkline counts={countsFor(issue.id)} label={issue.title} />
                    <div className="w-16 text-right text-xs text-muted">
                      <div className="tabular-nums">
                        {issue.times.toLocaleString()}×
                      </div>
                      <div className="mt-1">
                        <Ago at={issue.last_seen} />
                      </div>
                    </div>
                  </div>
                </Link>
              </li>
            ))}
          </ul>

          {page?.next_cursor ? (
            <div className="pt-4">
              <Button
                variant="quiet"
                onClick={() => void loadMore()}
                disabled={loadingMore}
              >
                {loadingMore ? "loading…" : "Load more"}
              </Button>
            </div>
          ) : null}
        </>
      )}
    </div>
  );
}
