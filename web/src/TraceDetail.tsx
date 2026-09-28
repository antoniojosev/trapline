// One request, span by span — the waterfall.
//
// Drawn as rows rather than as one SVG. A waterfall is a list of named,
// nested, selectable things with times attached, and that is a list: it wraps
// on a narrow screen, it reads in order to a screen reader, and the text is
// selectable so a span's description can be pasted into a ticket. The bar
// beside each row is the only graphic, and it is the one thing an SVG is
// better at than a div — a proportion.
//
// A trace is stored whole or not at all, because the sampling decision is a
// hash of the trace id and every transaction of one trace shares it. The gaps
// of the alternative look exactly like instrumentation nobody added
// (ADR 021), which is why this screen never has to say "part of this trace
// was not kept".

import { useEffect, useState } from "react";
import { ApiError, api, type Span, type Trace } from "./api";
import { formatMS } from "./Performance";
import { Link, performancePath, projectsPath } from "./routes";
import { Ago, Header, Notice, ProjectNav } from "./ui";

export function TraceDetail({
  projectID,
  traceID,
  onSignedOut,
}: {
  projectID: number;
  traceID: string;
  onSignedOut: () => void;
}) {
  const [trace, setTrace] = useState<Trace | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let current = true;
    setLoading(true);
    (async () => {
      try {
        const loaded = await api.getTrace(projectID, traceID);
        if (!current) return;
        setTrace(loaded);
        setError("");
      } catch (err) {
        if (!current) return;
        if (err instanceof ApiError && err.isUnauthorized) {
          onSignedOut();
          return;
        }
        setError(err instanceof Error ? err.message : "could not load the trace");
      } finally {
        if (current) setLoading(false);
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, traceID, onSignedOut]);

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <Header onSignedOut={onSignedOut}>
        <Link to={projectsPath} className="font-normal text-muted hover:text-accent">
          Projects
        </Link>
        <span className="px-2 text-muted">/</span>
        <Link
          to={performancePath(projectID)}
          className="font-normal text-muted hover:text-accent"
        >
          Performance
        </Link>
        <span className="px-2 text-muted">/</span>
        <span className="font-mono">trace</span>
      </Header>

      <ProjectNav projectID={projectID} current="performance" />

      <Notice>{error}</Notice>

      {loading ? (
        <p className="text-sm text-muted">loading…</p>
      ) : !trace ? (
        !error ? (
          <p className="text-sm text-muted">
            No such trace. On a sampled project that usually means the waterfall
            was not kept rather than that the request never happened — the
            aggregates counted it either way.
          </p>
        ) : null
      ) : (
        <>
          <section className="space-y-2">
            <h2 className="font-mono text-sm font-medium break-all">
              <Link
                to={performancePath(projectID, trace.transaction)}
                className="hover:text-accent"
              >
                {trace.transaction}
              </Link>
            </h2>
            <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted">
              <span className="tabular-nums">{formatMS(trace.duration_ms)}</span>
              <span>
                <Ago at={trace.timestamp} />
              </span>
              {trace.op && <span className="font-mono">{trace.op}</span>}
              {/* The status is written out rather than coloured, for the
                  reason every chart in this panel gives: colour alone says
                  nothing in a screenshot. */}
              <span className={isFailed(trace.status) ? "text-red-600" : ""}>
                {trace.status || "ok"}
              </span>
              <span className="font-mono break-all">{trace.trace_id}</span>
            </p>
          </section>

          <Waterfall trace={trace} />
        </>
      )}
    </div>
  );
}

// A row per span, ordered by start and indented by nesting.
//
// The tree is rebuilt here from `parent_span_id` rather than trusting the
// order spans arrived in: SDKs send them as they finish, so the deepest,
// fastest span is routinely first on the wire. A span whose parent is not in
// the trace is drawn at the root — that happens when a trace crosses services
// and only one of them reported, and hiding those rows would hide the half of
// the request that was recorded.
function Waterfall({ trace }: { trace: Trace }) {
  const spans = orderSpans(trace.spans);
  if (spans.length === 0) {
    return (
      <p className="text-sm text-muted">
        This transaction reported no spans — only its own duration. Instrument
        the calls inside it and they appear here.
      </p>
    );
  }

  const start = Math.min(...spans.map((entry) => entry.startMS));
  const end = Math.max(...spans.map((entry) => entry.endMS));
  const width = Math.max(end - start, 1);

  return (
    <section className="space-y-1">
      <h2 className="text-sm font-medium">
        Waterfall{" "}
        <span className="font-normal text-muted">
          ({spans.length} {spans.length === 1 ? "span" : "spans"})
        </span>
      </h2>
      <ol className="space-y-0.5">
        {spans.map((entry) => {
          const offset = ((entry.startMS - start) / width) * 100;
          const span = ((entry.endMS - entry.startMS) / width) * 100;
          return (
            <li
              key={entry.key}
              className="grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-3 gap-y-0.5 rounded px-1 py-1 text-xs hover:bg-line/30 sm:grid-cols-[minmax(0,14rem)_minmax(0,1fr)_auto]"
            >
              <span
                className="min-w-0 truncate"
                style={{ paddingLeft: `${Math.min(entry.depth, 6) * 0.75}rem` }}
                title={`${entry.span.op ?? ""} ${entry.span.description ?? ""}`.trim()}
              >
                <span className="font-mono">{entry.span.op || "span"}</span>
                {entry.span.description && (
                  <span className="text-muted"> {entry.span.description}</span>
                )}
              </span>

              {/* The bar is in its own column so every row's timeline starts
                  at the same x — indenting the bar with the label would make
                  a deep span look late. */}
              <svg
                viewBox="0 0 100 6"
                preserveAspectRatio="none"
                className="order-last col-span-2 h-1.5 w-full sm:order-none sm:col-span-1"
                role="img"
                aria-label={`starts ${formatMS(entry.startMS - start)} in, lasts ${formatMS(entry.endMS - entry.startMS)}`}
              >
                <rect x="0" y="1" width="100" height="4" fill="var(--chart-track)" rx="1" />
                <rect
                  x={offset}
                  y="0"
                  width={Math.max(span, 0.5)}
                  height="6"
                  fill={
                    isFailed(entry.span.status)
                      ? "var(--chart-error)"
                      : "var(--chart-bar)"
                  }
                  rx="1"
                />
              </svg>

              <span className="shrink-0 tabular-nums text-muted">
                {formatMS(entry.endMS - entry.startMS)}
              </span>
            </li>
          );
        })}
      </ol>
    </section>
  );
}

interface Placed {
  key: string;
  span: Span;
  depth: number;
  startMS: number;
  endMS: number;
}

/**
 * orderSpans turns the flat list into depth-first order with a nesting level.
 *
 * Iterative rather than recursive on purpose: the depth of a trace is decided
 * by whoever instrumented the service, a recursive walk over a pathological
 * one would overflow the stack, and a panel that crashes on the one slow
 * request somebody came to look at is worse than one that draws it flat.
 * A cycle — which a malformed payload can contain — is broken by only ever
 * visiting a span once.
 */
function orderSpans(spans: Span[]): Placed[] {
  const children = new Map<string, Span[]>();
  const known = new Set<string>();
  for (const span of spans) {
    if (span.span_id) known.add(span.span_id);
  }
  const roots: Span[] = [];
  for (const span of spans) {
    const parent = span.parent_span_id;
    if (parent && known.has(parent) && parent !== span.span_id) {
      const siblings = children.get(parent) ?? [];
      siblings.push(span);
      children.set(parent, siblings);
      continue;
    }
    roots.push(span);
  }

  const byStart = (left: Span, right: Span) =>
    millis(left.start) - millis(right.start);
  roots.sort(byStart);
  for (const siblings of children.values()) siblings.sort(byStart);

  const placed: Placed[] = [];
  const visited = new Set<Span>();
  const stack: { span: Span; depth: number }[] = roots
    .map((span, index) => ({ span, depth: 0, index }))
    .reverse();

  while (stack.length > 0) {
    const next = stack.pop();
    if (!next || visited.has(next.span)) continue;
    visited.add(next.span);

    const startMS = millis(next.span.start);
    placed.push({
      // A span id is the natural key, and some SDKs leave it out; the index
      // is what keeps those rows distinct instead of collapsing them.
      key: next.span.span_id || `${placed.length}`,
      span: next.span,
      depth: next.depth,
      startMS,
      endMS: startMS + (next.span.duration_ms || 0),
    });

    const kids = next.span.span_id ? children.get(next.span.span_id) : undefined;
    if (!kids) continue;
    for (let index = kids.length - 1; index >= 0; index--) {
      const kid = kids[index];
      if (kid) stack.push({ span: kid, depth: next.depth + 1 });
    }
  }
  return placed;
}

function millis(at: string): number {
  const parsed = Date.parse(at);
  return Number.isNaN(parsed) ? 0 : parsed;
}

/**
 * isFailed mirrors the server's rule, which is not "status is not ok".
 *
 * A cancelled request is the client hanging up and an unknown one is an SDK
 * that did not say; counting either as a failure turns every page somebody
 * navigated away from into an error rate (ADR 021).
 */
function isFailed(status?: string): boolean {
  // Trimmed and lower-cased like the server does, and an empty status is not
  // a failure: several SDKs omit it for a transaction that went fine, and
  // reading absence as failure would put those services at a hundred percent
  // the day tracing is switched on.
  const normalised = (status ?? "").trim().toLowerCase();
  if (normalised === "") return false;
  return (
    normalised !== "ok" &&
    normalised !== "cancelled" &&
    normalised !== "unknown"
  );
}
