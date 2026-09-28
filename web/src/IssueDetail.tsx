import type { ReactNode } from "react";
import { useEffect, useState } from "react";
import {
  ApiError,
  api,
  type IssueDetail as Detail,
  type IssueSeries,
  type IssueStatus,
  type Project,
  type ReleaseCommit,
  type StoredEvent,
  type Suspect,
  type SuspectsReport,
  type TagCount,
} from "./api";
import { Sparkline } from "./charts";
import {
  REDACTED,
  callOrder,
  framePath,
  hasRedaction,
  minified,
  symbolicated,
  traces,
  type Breadcrumb,
  type Frame,
  type Trace,
} from "./event";
import { Link, issuesPath, projectsPath, releasePath } from "./routes";
import { Ago, Button, Header, LevelTag, Notice, ReleaseTag, StatusTag } from "./ui";

export function IssueDetail({
  projectID,
  issueID,
  onSignedOut,
}: {
  projectID: number;
  issueID: number;
  onSignedOut: () => void;
}) {
  const [project, setProject] = useState<Project | null>(null);
  const [detail, setDetail] = useState<Detail | null>(null);
  const [series, setSeries] = useState<IssueSeries | null>(null);
  const [suspects, setSuspects] = useState<SuspectsReport | null>(null);
  const [selected, setSelected] = useState(0);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let current = true;
    setLoading(true);
    (async () => {
      try {
        const [loadedProject, loadedDetail] = await Promise.all([
          api.getProject(projectID),
          api.getIssue(projectID, issueID),
        ]);
        if (!current) return;
        setProject(loadedProject);
        setDetail(loadedDetail);
        setSelected(0);
        setError("");
      } catch (err) {
        if (!current) return;
        if (err instanceof ApiError && err.isUnauthorized) {
          onSignedOut();
          return;
        }
        setError(err instanceof Error ? err.message : "could not load the issue");
      } finally {
        if (current) setLoading(false);
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, issueID, onSignedOut]);

  // The issue's own last day, loaded separately because it is a chart beside
  // the facts rather than one of them: if it fails, the page someone opened to
  // fix a bug still shows the stacktrace.
  useEffect(() => {
    let current = true;
    setSeries(null);
    (async () => {
      try {
        const loaded = await api.issueSeries(projectID, issueID);
        if (current) setSeries(loaded);
      } catch {
        // A missing chart is a missing chart.
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, issueID]);

  // Which change probably caused this, loaded separately for the reason the
  // chart is: it reads a release's whole commit set and decodes a stored
  // payload, and if it fails the page somebody opened to fix a bug still
  // shows the stacktrace (ADR 019).
  useEffect(() => {
    let current = true;
    setSuspects(null);
    (async () => {
      try {
        const loaded = await api.issueSuspects(projectID, issueID);
        if (current) setSuspects(loaded);
      } catch {
        // A missing guess is a missing guess.
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, issueID]);

  async function triage(status: IssueStatus, inNextRelease = false) {
    setBusy(true);
    setError("");
    try {
      await api.setIssueStatus(projectID, issueID, status, inNextRelease);
      // Re-read rather than patching the status in place. Resolving writes six
      // more columns than the one the response names — which release it was
      // pinned to, when, and the suppression counter that resets — and a screen
      // that guessed at them would be telling the reader something the database
      // does not say.
      const refreshed = await api.getIssue(projectID, issueID);
      setDetail(refreshed);
    } catch (err) {
      setError(err instanceof Error ? err.message : "could not change the status");
    } finally {
      setBusy(false);
    }
  }

  const event = detail?.events[selected];

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
        <Link
          to={issuesPath(projectID)}
          className="font-normal text-muted hover:text-accent"
        >
          {project?.name ?? `#${projectID}`}
        </Link>
        <span className="px-2 text-muted">/</span>
        <span className="text-muted">#{issueID}</span>
      </Header>

      <Notice>{error}</Notice>

      {loading ? (
        <p className="text-sm text-muted">loading…</p>
      ) : !detail ? null : (
        <>
          <Summary
            detail={detail}
            series={series}
            projectID={projectID}
            busy={busy}
            onTriage={triage}
          />

          {suspects && <Suspects report={suspects} projectID={projectID} />}

          {detail.events.length === 0 ? (
            <p className="text-sm text-muted">
              This issue has no stored occurrences left. Retention removes
              events long before it removes the issue they grouped into.
            </p>
          ) : (
            <>
              <Occurrences
                events={detail.events}
                total={detail.times}
                selected={selected}
                onSelect={setSelected}
              />
              {event && (
                <Occurrence
                  event={event}
                  tags={detail.tags}
                  projectID={projectID}
                />
              )}
            </>
          )}
        </>
      )}
    </div>
  );
}

function Summary({
  detail,
  series,
  projectID,
  busy,
  onTriage,
}: {
  detail: Detail;
  series: IssueSeries | null;
  projectID: number;
  busy: boolean;
  onTriage: (status: IssueStatus, inNextRelease?: boolean) => void;
}) {
  const primary = traces(detail.events[0]?.payload ?? {})[0]?.exception;
  const counts = series?.series.map((point) => point.count) ?? [];

  return (
    <section className="space-y-3">
      <div className="flex items-center gap-2">
        <LevelTag level={detail.level} />
        <StatusTag status={detail.status} />
      </div>

      <div>
        <h2 className="text-lg font-semibold break-words">
          {primary?.type ?? detail.title}
        </h2>
        {primary?.value && (
          <p className="mt-1 text-sm break-words">{primary.value}</p>
        )}
        {detail.culprit && (
          <p className="mt-1 font-mono text-xs text-muted break-all">
            {detail.culprit}
          </p>
        )}
      </div>

      <Lifecycle detail={detail} projectID={projectID} />

      <div className="flex items-end justify-between gap-4">
        <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted">
          <span className="tabular-nums">
            {detail.times.toLocaleString()}{" "}
            {detail.times === 1 ? "occurrence" : "occurrences"}
          </span>
          <span>
            first seen <Ago at={detail.first_seen} />
          </span>
          <span>
            last seen <Ago at={detail.last_seen} />
          </span>
        </p>
        {counts.length > 0 && (
          <div className="shrink-0 text-right">
            <Sparkline
              counts={counts}
              label="This issue"
              className="h-8 w-32"
            />
            <p className="text-xs text-muted tabular-nums">
              {(series?.total ?? 0).toLocaleString()} in 24h
            </p>
          </div>
        )}
      </div>

      <div className="flex flex-wrap gap-2">
        {detail.status !== "resolved" && (
          <Button disabled={busy} onClick={() => onTriage("resolved")}>
            Resolve
          </Button>
        )}
        {/* The feature with the most value in the phase, and it needs a
            release to pin to: without one there is nothing to compare a later
            event against, and the button would silently do what plain Resolve
            already does. */}
        {detail.status !== "resolved" && detail.last_release && (
          <Button
            variant="quiet"
            disabled={busy}
            onClick={() => onTriage("resolved", true)}
          >
            Resolve in next release
          </Button>
        )}
        {detail.status !== "ignored" && (
          <Button
            variant="quiet"
            disabled={busy}
            onClick={() => onTriage("ignored")}
          >
            Ignore
          </Button>
        )}
        {detail.status !== "unresolved" && (
          <Button
            variant="quiet"
            disabled={busy}
            onClick={() => onTriage("unresolved")}
          >
            Reopen
          </Button>
        )}
        <CopyBundle projectID={projectID} issueID={detail.id} />
      </div>

      {detail.status === "resolved" && !detail.resolve_next_release && (
        <p className="text-xs text-muted">
          A new event reopens this as a regression.
        </p>
      )}
      {detail.status === "resolved" && detail.resolve_next_release && (
        <p className="text-xs text-muted">
          Resolved in the next release after{" "}
          <ReleaseTag version={detail.resolved_in_release} />. Events still
          arriving from that build are expected — the machines running it have
          not been replaced yet — so they count without reopening this.
          {detail.seen_in_resolved_release_count > 0 && (
            <>
              {" "}
              <strong className="font-medium text-ink">
                {detail.seen_in_resolved_release_count.toLocaleString()}
              </strong>{" "}
              {detail.seen_in_resolved_release_count === 1 ? "has" : "have"}{" "}
              arrived so far and did not.
            </>
          )}
        </p>
      )}
      {detail.status === "ignored" && (
        <p className="text-xs text-muted">
          New events still count, but they will not reopen this.
        </p>
      )}
    </section>
  );
}

/**
 * Lifecycle answers the two questions the whole phase was built for: is this
 * new or is it back, and which release brought it.
 *
 * A banner rather than three more rows of a facts list, because during an
 * incident it is the first thing that has to be read and the last thing that
 * should need looking for. A regression is stated as a regression, in words,
 * naming the release — not implied by a status that reverted to "unresolved"
 * and a number in a corner.
 */
function Lifecycle({
  detail,
  projectID,
}: {
  detail: Detail;
  projectID: number;
}) {
  const regressed = detail.regressions > 0;
  if (!regressed && !detail.first_release) return null;

  return (
    <div
      className={`rounded-md border px-3 py-2 text-sm ${
        regressed && detail.status === "unresolved"
          ? "border-amber-500/50 bg-amber-500/10"
          : "border-line"
      }`}
    >
      {regressed ? (
        <p>
          <strong className="font-medium">
            Regression{detail.regressions > 1 ? ` ×${detail.regressions}` : ""}
          </strong>{" "}
          — this was resolved and came back
          {detail.regressed_in_release ? (
            <>
              {" "}
              in{" "}
              <Link
                to={releasePath(projectID, detail.regressed_in_release)}
                className="font-mono underline decoration-dotted hover:text-accent"
              >
                {detail.regressed_in_release}
              </Link>
            </>
          ) : null}
          .
        </p>
      ) : (
        <p>
          <strong className="font-medium">First seen</strong> in{" "}
          {detail.first_release ? (
            <Link
              to={releasePath(projectID, detail.first_release)}
              className="font-mono underline decoration-dotted hover:text-accent"
            >
              {detail.first_release}
            </Link>
          ) : (
            <ReleaseTag version={undefined} />
          )}
          .
        </p>
      )}

      <p className="mt-1 flex flex-wrap gap-x-3 text-xs text-muted">
        {detail.first_release && regressed && (
          <span>
            first seen in{" "}
            <Link
              to={releasePath(projectID, detail.first_release)}
              className="font-mono hover:text-accent"
            >
              {detail.first_release}
            </Link>
          </span>
        )}
        <span>
          last seen in <ReleaseTag version={detail.last_release} />
        </span>
        {detail.resolved_at && (
          <span>
            resolved <Ago at={detail.resolved_at} />
          </span>
        )}
      </p>
    </div>
  );
}

const VISIBLE_OCCURRENCES = 5;

function Occurrences({
  events,
  total,
  selected,
  onSelect,
}: {
  events: StoredEvent[];
  total: number;
  selected: number;
  onSelect: (index: number) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const shown = expanded ? events : events.slice(0, VISIBLE_OCCURRENCES);

  return (
    <Section
      title="Recent occurrences"
      note={
        total > events.length
          ? `the ${events.length} most recent of ${total.toLocaleString()}`
          : undefined
      }
      action={
        events.length > VISIBLE_OCCURRENCES ? (
          <QuietToggle
            expanded={expanded}
            onClick={() => setExpanded(!expanded)}
          >
            {expanded ? "show fewer" : `show all ${events.length}`}
          </QuietToggle>
        ) : undefined
      }
    >
      <ul>
        {shown.map((event, index) => (
          <li key={event.id}>
            <button
              type="button"
              aria-current={index === selected}
              onClick={() => onSelect(index)}
              className={`flex w-full items-baseline gap-3 rounded border-l-2 px-2 py-1.5 text-left text-xs transition ${
                index === selected
                  ? "border-accent bg-line/40"
                  : "border-transparent text-muted hover:bg-line/20"
              }`}
            >
              <span className="w-24 shrink-0 tabular-nums sm:w-28">
                <Ago at={event.occurred_at} />
              </span>
              <span className="w-28 shrink-0 truncate font-mono sm:w-40">
                {event.release || "no release"}
              </span>
              <span className="truncate">
                {event.environment || "no environment"}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </Section>
  );
}

function Occurrence({
  event,
  tags,
  projectID,
}: {
  event: StoredEvent;
  tags: Record<string, TagCount[]>;
  projectID: number;
}) {
  const payload = event.payload;
  const stacks = traces(payload);
  const blocks: { label: string; data: Record<string, unknown> | undefined }[] =
    [
      // Directly after the aggregate above, so the two tag lists read as the
      // two questions they answer rather than as one accidental repetition.
      { label: "Tags on this occurrence", data: payload.tags },
      { label: "Request", data: payload.request },
      { label: "User", data: payload.user },
      { label: "Contexts", data: payload.contexts },
      { label: "Extra", data: payload.extra },
    ];

  return (
    <>
      <Section
        title="Occurrence"
        note={new Date(event.occurred_at).toLocaleString()}
      >
        <dl className="grid grid-cols-[5rem_1fr] sm:grid-cols-[8rem_1fr] gap-x-3 gap-y-1 text-xs">
          <Row label="Event">
            <span className="font-mono break-all">{event.event_id}</span>
          </Row>
          {payload.transaction && (
            <Row label="Transaction">{payload.transaction}</Row>
          )}
          {event.release && <Row label="Release">{event.release}</Row>}
          {event.environment && (
            <Row label="Environment">{event.environment}</Row>
          )}
          {payload.server_name && (
            <Row label="Server">{payload.server_name}</Row>
          )}
          {payload.platform && <Row label="Platform">{payload.platform}</Row>}
          <Row label="Received">
            <Ago at={event.received_at} />
          </Row>
        </dl>
        {payload.message && (
          <p className="pt-2 text-sm break-words">{payload.message}</p>
        )}
      </Section>

      {stacks.length > 0 && <Stacktraces stacks={stacks} />}

      {payload.breadcrumbs && payload.breadcrumbs.length > 0 && (
        <Breadcrumbs crumbs={payload.breadcrumbs} />
      )}

      {Object.keys(tags).length > 0 && (
        <Tags tags={tags} projectID={projectID} />
      )}

      {blocks.map(({ label, data }) =>
        data && Object.keys(data).length > 0 ? (
          <Section key={label} title={label}>
            <Data value={data} depth={0} />
          </Section>
        ) : null,
      )}

      {hasRedaction(payload) && (
        <p className="border-t border-line pt-4 text-xs text-muted">
          <Redacted /> marks a value the scrubber removed before this event was
          written to disk. The secret never reached storage, so there is
          nothing here to recover — and nothing here to leak.
        </p>
      )}
    </>
  );
}

/**
 * writeToClipboard, with the fallback that makes it work where this product
 * actually runs.
 *
 * `navigator.clipboard` exists only in a secure context, and "secure" means
 * HTTPS or localhost — so it is missing on exactly the deployment this product
 * recommends first: a binary on a VPS, reached by its hostname over plain HTTP
 * while somebody is still setting up a certificate. A button that worked in
 * development and silently failed there would be the worst version of this
 * feature.
 *
 * The fallback is the deprecated `document.execCommand("copy")` over a
 * throwaway textarea, which is the only clipboard API a non-secure context
 * has. It is deprecated, not removed, and it is reached only when the modern
 * one is absent.
 */
async function writeToClipboard(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Some browsers refuse outright when the page is not focused. Fall
    // through rather than giving up: the older path often still works.
  }

  const scratch = document.createElement("textarea");
  scratch.value = text;
  // Off-screen rather than hidden: an element with `display: none` is not
  // selectable, and a selection is what execCommand copies.
  scratch.style.position = "fixed";
  scratch.style.left = "-9999px";
  document.body.appendChild(scratch);
  scratch.select();
  try {
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    document.body.removeChild(scratch);
  }
}

/**
 * CopyBundle puts the whole issue on the clipboard as markdown.
 *
 * The panel does not render the bundle: every fact in it is already on this
 * screen, laid out better than a document can. What this button is for is
 * getting it *out* — an operator who wants to hand the issue to an agent
 * should not have to find an API token and write a curl command, which is the
 * step that decides whether the agent-first story is real or a paragraph in a
 * README (ADR 006, ADR 022).
 *
 * The document is fetched on click rather than with the page. It is the one
 * thing on this screen nobody reads unless they ask for it, and loading it
 * eagerly would spend a request, a stacktrace decode and a commit-set read on
 * every visit for the sake of a button most visits do not press.
 */
function CopyBundle({
  projectID,
  issueID,
}: {
  projectID: number;
  issueID: number;
}) {
  const [state, setState] = useState<"idle" | "busy" | "copied" | "failed">(
    "idle",
  );

  async function copy() {
    setState("busy");
    try {
      const bundle = await api.issueBundle(projectID, issueID);
      if (!(await writeToClipboard(bundle))) {
        setState("failed");
        return;
      }
      setState("copied");
      // Back to the resting label, so the button does not keep claiming a
      // success that belongs to a click from ten minutes ago.
      window.setTimeout(() => setState("idle"), 2000);
    } catch {
      // The request itself failed — no session, no such issue, no server.
      setState("failed");
    }
  }

  return (
    <Button variant="quiet" disabled={state === "busy"} onClick={copy}>
      {state === "copied"
        ? "Copied"
        : state === "failed"
          ? "Could not copy"
          : "Copy for an agent"}
    </Button>
  );
}

/**
 * Suspects is the answer to "which change probably caused this?".
 *
 * It sits above the stacktrace because it is what the reader came for, and it
 * renders its own emptiness rather than disappearing: "no commits were sent",
 * "they arrived without file paths" and "these frames are still minified" are
 * each a thing the reader can go and fix, and a section that vanished would
 * have told them nothing was wrong (ADR 019).
 */
function Suspects({
  report,
  projectID,
}: {
  report: SuspectsReport;
  projectID: number;
}) {
  // Nothing at all to say: no release, no commits, no warning worth a box.
  if (
    report.suspects.length === 0 &&
    report.commits.length === 0 &&
    !report.warning
  ) {
    return null;
  }

  return (
    <Section
      title="Suspect commits"
      note={report.release ? `from ${report.release}` : undefined}
    >
      {report.suspects.length > 0 ? (
        <ol className="space-y-2">
          {report.suspects.map((suspect, index) => (
            <li key={suspect.id}>
              <SuspectLine suspect={suspect} rank={index + 1} />
            </li>
          ))}
        </ol>
      ) : (
        <ul className="space-y-1">
          {report.commits.map((commit) => (
            <li key={commit.id} className="text-xs">
              <CommitLine commit={commit} />
            </li>
          ))}
          {report.commits.length < report.commit_count && (
            <li className="text-xs text-muted">
              and {report.commit_count - report.commits.length} more in{" "}
              <Link
                to={releasePath(projectID, report.release ?? "")}
                className="underline decoration-dotted hover:text-accent"
              >
                the release
              </Link>
            </li>
          )}
        </ul>
      )}

      {report.warning && (
        <p className="text-xs text-muted">{report.warning}</p>
      )}
    </Section>
  );
}

/** One accused commit, with the evidence underneath it. */
function SuspectLine({ suspect, rank }: { suspect: Suspect; rank: number }) {
  return (
    <div
      className={`rounded-md border p-2 ${
        rank === 1 ? "border-amber-500/50 bg-amber-500/10" : "border-line"
      }`}
    >
      <CommitLine commit={suspect} />
      <ul className="mt-1 space-y-0.5">
        {suspect.reasons.map((reason) => (
          <li key={`${reason.path}:${reason.frame_depth}`} className="text-xs text-muted">
            touched <span className="font-mono text-ink">{reason.path}</span>
            {reason.type === "D" ? " (deleted)" : reason.type === "A" ? " (added)" : ""}
            , which is frame #{reason.frame_depth + 1}
          </li>
        ))}
      </ul>
    </div>
  );
}

/** A commit as one line: the short sha, the subject and who wrote it. */
function CommitLine({ commit }: { commit: ReleaseCommit }) {
  return (
    <>
      <div className="flex items-baseline gap-2 text-xs">
        {/* The short sha, because that is what anyone types into `git show`.
            The full one is in the title for copying. */}
        <code className="shrink-0 font-mono text-muted" title={commit.id}>
          {commit.id.slice(0, 8)}
        </code>
        <span className="min-w-0 truncate">
          {(commit.message ?? "").split("\n", 1)[0] || "no message"}
        </span>
      </div>
      {(commit.author_name || commit.timestamp) && (
        <p className="mt-0.5 flex flex-wrap gap-x-2 text-xs text-muted">
          {commit.author_name && <span>{commit.author_name}</span>}
          {commit.timestamp && <Ago at={commit.timestamp} />}
        </p>
      )}
    </>
  );
}

function Stacktraces({ stacks }: { stacks: Trace[] }) {
  const [showLibrary, setShowLibrary] = useState(false);
  // Off by default: the resolved frame is where the bug is, and the minified
  // one is what you switch to when you suspect the map itself (ADR 018).
  const [showRaw, setShowRaw] = useState(false);
  const library = stacks.reduce(
    (count, stack) => count + hideable(stack.frames).length,
    0,
  );
  const resolved = stacks.some((stack) => symbolicated(stack.frames));

  return (
    <Section
      title="Stacktrace"
      note={
        resolved && !showRaw
          ? "most recent call first, resolved from source maps"
          : "most recent call first"
      }
      action={
        library > 0 || resolved ? (
          <span className="flex shrink-0 gap-3">
            {resolved && (
              <QuietToggle expanded={showRaw} onClick={() => setShowRaw(!showRaw)}>
                {showRaw ? "show original source" : "show minified"}
              </QuietToggle>
            )}
            {library > 0 && (
              <QuietToggle
                expanded={showLibrary}
                onClick={() => setShowLibrary(!showLibrary)}
              >
                {showLibrary
                  ? "hide library frames"
                  : `show ${library} library ${library === 1 ? "frame" : "frames"}`}
              </QuietToggle>
            )}
          </span>
        ) : undefined
      }
    >
      {stacks.map((stack, index) => (
        <div key={index} className="space-y-2">
          {index > 0 && (
            <p className="pt-2 text-xs text-muted">
              caused by{" "}
              <span className="font-medium text-ink">
                {stack.exception?.type}
              </span>
              {stack.exception?.value ? `: ${stack.exception.value}` : ""}
            </p>
          )}
          <Frames
            frames={stack.frames}
            showLibrary={showLibrary}
            showRaw={showRaw}
          />
        </div>
      ))}
    </Section>
  );
}

function isLibrary(frame: Frame): boolean {
  return frame.in_app !== true;
}

/**
 * hideable is the library frames a stacktrace can collapse.
 *
 * None of them, if that is all the stacktrace has. Hiding those would leave
 * the reader looking at nothing, which is worse than looking at a frame from
 * someone else's code — and a wrapped cause is very often exactly that.
 */
function hideable(frames: Frame[]): Frame[] {
  return frames.some((frame) => !isLibrary(frame))
    ? frames.filter(isLibrary)
    : [];
}

function Frames({
  frames,
  showLibrary,
  showRaw,
}: {
  frames: Frame[];
  showLibrary: boolean;
  showRaw: boolean;
}) {
  const ordered = callOrder(frames);
  const collapsed = !showLibrary && hideable(frames).length > 0;
  const visible = collapsed
    ? ordered.filter((frame) => !isLibrary(frame))
    : ordered;

  if (visible.length === 0) {
    return <p className="text-xs text-muted">No frames.</p>;
  }

  return (
    <ol className="space-y-1">
      {visible.map((frame, index) => (
        <li key={index}>
          {/* A frame that was never symbolicated has no minified form to
              switch to, and it stays as it is rather than disappearing: a
              stacktrace that half empties when a toggle is pressed reads as
              a bug in the panel. */}
          <FrameLine
            frame={(showRaw && minified(frame)) || frame}
            failing={index === 0}
          />
        </li>
      ))}
    </ol>
  );
}

/**
 * FrameLine renders one frame, and the failing one differently.
 *
 * The first frame after the reversal is the call that actually raised. It is
 * what the reader opened the page for, so it gets the source context and the
 * rest get one line each.
 */
function FrameLine({ frame, failing }: { frame: Frame; failing: boolean }) {
  const path = framePath(frame);
  const body = (
    <p className={`text-sm break-all ${isLibrary(frame) ? "text-muted" : ""}`}>
      <span className="font-mono text-xs">{path || "<unknown file>"}</span>
      {frame.function && (
        <>
          <span className="text-muted"> in </span>
          <span className="font-medium">{frame.function}</span>
        </>
      )}
      {frame.lineno !== undefined && frame.lineno > 0 && (
        <span className="text-muted"> line {frame.lineno}</span>
      )}
    </p>
  );

  if (!failing) return body;

  return (
    <div className="rounded-md border border-line bg-line/20 p-2">
      {body}
      <SourceContext frame={frame} />
    </div>
  );
}

function SourceContext({ frame }: { frame: Frame }) {
  if (!frame.context_line) return null;

  const before = frame.pre_context ?? [];
  const after = frame.post_context ?? [];
  const first = (frame.lineno ?? before.length + 1) - before.length;
  const lines = [
    ...before.map((text, index) => ({ text, failing: false, index })),
    { text: frame.context_line, failing: true, index: before.length },
    ...after.map((text, index) => ({
      text,
      failing: false,
      index: before.length + 1 + index,
    })),
  ];

  return (
    <pre className="mt-2 overflow-x-auto rounded bg-surface p-2 font-mono text-xs">
      {lines.map((line) => (
        <div
          key={line.index}
          className={line.failing ? "bg-red-500/10 font-medium" : "text-muted"}
        >
          <span className="mr-3 inline-block w-10 text-right tabular-nums select-none">
            {first + line.index}
          </span>
          {line.text}
        </div>
      ))}
    </pre>
  );
}

function Breadcrumbs({ crumbs }: { crumbs: Breadcrumb[] }) {
  return (
    <Section title="Breadcrumbs" note="oldest first, the error last">
      <ol className="space-y-1.5">
        {crumbs.map((crumb, index) => (
          <li key={index} className="flex gap-3 text-xs">
            <span className="w-24 shrink-0 font-mono tabular-nums text-muted">
              {crumb.timestamp ? clockTime(crumb.timestamp) : "—"}
            </span>
            <span className="w-20 shrink-0 truncate text-muted">
              {crumb.category ?? crumb.type ?? ""}
            </span>
            <div className="min-w-0 flex-1">
              <p
                className={`break-words ${
                  crumb.level === "error" || crumb.level === "fatal"
                    ? "text-red-600 dark:text-red-400"
                    : crumb.level === "warning"
                      ? "text-amber-600 dark:text-amber-400"
                      : ""
                }`}
              >
                {crumb.message || <span className="text-muted">no message</span>}
              </p>
              {crumb.data && Object.keys(crumb.data).length > 0 && (
                // Inline pairs rather than the key/value grid used elsewhere:
                // a breadcrumb's data is a handful of scalars in a column
                // already narrowed by the time and category, and a nested
                // grid in there has nowhere left to put the value.
                <p className="mt-0.5 flex flex-wrap gap-x-3 text-muted">
                  {Object.entries(crumb.data).map(([key, value]) => (
                    <span key={key}>
                      {key}
                      <span className="px-0.5">=</span>
                      <Data value={value} depth={MAX_DEPTH} />
                    </span>
                  ))}
                </p>
              )}
            </div>
          </li>
        ))}
      </ol>
    </Section>
  );
}

/**
 * Tags, and every value is a way into the list.
 *
 * A tag that can be read but not filtered on answers "what was the browser
 * here" and leaves "does this only happen in Safari?" unanswerable from the
 * panel — which is the difference between a fact and a question. Each value
 * links to the listing already narrowed to it, so the answer is one click and
 * a shareable address.
 */
function Tags({
  tags,
  projectID,
}: {
  tags: Record<string, TagCount[]>;
  projectID: number;
}) {
  const keys = Object.keys(tags).sort();

  return (
    <Section title="Tags" note="across the stored occurrences">
      <dl className="grid grid-cols-[5rem_1fr] sm:grid-cols-[8rem_1fr] gap-x-3 gap-y-1 text-xs">
        {keys.map((key) => (
          <Row key={key} label={key}>
            <span className="flex flex-wrap gap-x-3 gap-y-1">
              {(tags[key] ?? []).map((tag) => (
                <span key={tag.value}>
                  {tag.value === REDACTED ? (
                    // A redacted value is not a filter: there is nothing
                    // behind it to match, and offering the link would promise
                    // a list that must always be empty.
                    <Redacted />
                  ) : (
                    <Link
                      to={issuesPath(projectID, { tag: `${key}:${tag.value}` })}
                      className="underline decoration-dotted underline-offset-2 hover:text-accent"
                      // The visible text is a bare value, which does not say
                      // what following it does.
                      label={`Show issues tagged ${key}: ${tag.value}`}
                    >
                      {tag.value}
                    </Link>
                  )}
                  {/* The count only earns its place when the tag does not
                      simply hold one value for every occurrence. */}
                  {(tags[key] ?? []).length > 1 && (
                    <span className="text-muted tabular-nums"> ×{tag.count}</span>
                  )}
                </span>
              ))}
            </span>
          </Row>
        ))}
      </dl>
    </Section>
  );
}

const MAX_DEPTH = 3;

/**
 * Data renders an arbitrary slice of the payload.
 *
 * The payload is whatever the SDK put in `extra` and `contexts`, so this has
 * to survive shapes nobody anticipated. Past a few levels it stops pretending
 * to be a table and prints the JSON, which is more honest than an indent
 * ladder marching off the right edge of the page.
 */
function Data({ value, depth }: { value: unknown; depth: number }): ReactNode {
  if (value === null || value === undefined) {
    return <span className="text-muted">null</span>;
  }
  if (typeof value === "string") return <ScalarValue value={value} />;
  if (typeof value === "number" || typeof value === "boolean") {
    return <span className="font-mono tabular-nums">{String(value)}</span>;
  }
  if (depth >= MAX_DEPTH) {
    return (
      <code className="font-mono break-all">{JSON.stringify(value)}</code>
    );
  }
  if (Array.isArray(value)) {
    if (value.length === 0) return <span className="text-muted">empty</span>;
    return (
      <ul className="space-y-0.5">
        {value.map((item, index) => (
          <li key={index}>
            <Data value={item} depth={depth + 1} />
          </li>
        ))}
      </ul>
    );
  }

  const entries = Object.entries(value as Record<string, unknown>);
  if (entries.length === 0) return <span className="text-muted">empty</span>;
  return (
    <dl className="grid grid-cols-[5rem_1fr] sm:grid-cols-[8rem_1fr] gap-x-3 gap-y-1 text-xs">
      {entries.map(([key, item]) => (
        <Row key={key} label={key}>
          <Data value={item} depth={depth + 1} />
        </Row>
      ))}
    </dl>
  );
}

function ScalarValue({ value }: { value: string }) {
  if (value === REDACTED) return <Redacted />;
  return <span className="break-all">{value}</span>;
}

/**
 * Redacted marks a removed value as a decision rather than a gap.
 *
 * Somebody reading a stacktrace at 3am needs to know instantly that nothing
 * is broken here and there is no lost data to go hunting for: the scrubber
 * did its job on the way in.
 */
function Redacted() {
  return (
    <span
      className="rounded border border-line px-1 font-mono text-muted"
      title="Removed by the PII scrubber before this event was stored"
    >
      {REDACTED}
    </span>
  );
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="truncate text-muted" title={label}>
        {label}
      </dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}

function Section({
  title,
  note,
  action,
  children,
}: {
  title: string;
  note?: string | undefined;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="space-y-2 border-t border-line pt-4">
      <div className="flex items-baseline justify-between gap-3">
        <h3 className="text-sm font-medium">
          {title}
          {/* The space is not decoration. Margin separates these two visually
              but leaves no character between them, so the heading's accessible
              name came out as "Stacktracemost recent call first" — one word
              nobody wrote, announced to the readers who have only that name to
              go on. */}
          {note && <span className="ml-2 text-xs text-muted"> {note}</span>}
        </h3>
        {action}
      </div>
      {children}
    </section>
  );
}

function QuietToggle({
  expanded,
  onClick,
  children,
}: {
  expanded: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      aria-expanded={expanded}
      onClick={onClick}
      className="shrink-0 text-xs text-muted hover:text-accent"
    >
      {children}
    </button>
  );
}

function clockTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}
