// "Which endpoint got slow after the deploy", which is the whole of the performance screen.
//
// One table, three sortings, and a series underneath when a row is opened.
// Every percentile on this screen was computed when it was asked for, by
// merging the sketches of the buckets in range (ADR 007, ADR 020). That is
// why changing the range changes p95 in a way that averaging stored
// percentiles never would, and why the summary under a chart is not the mean
// of the points above it.
//
// The sampling line is not decoration. A reader who sees "12 340 requests"
// and "40 traces" needs to be told which of the two was sampled before they
// compare either against a load balancer and conclude this product is losing
// data. The aggregates cover everything received; sampling only decides
// whether a waterfall was kept (ADR 021).

import type { ReactNode } from "react";
import { useEffect, useState } from "react";
import {
  ApiError,
  api,
  type DashboardRange,
  type Trace,
  type TransactionList,
  type TransactionSeries,
  type TransactionSort,
  type TransactionSummary,
} from "./api";
import { LatencyLines } from "./charts";
import {
  Link,
  navigate,
  performancePath,
  projectsPath,
  tracePath,
} from "./routes";
import { Ago, Header, Notice, ProjectNav } from "./ui";

const RANGES: { key: DashboardRange; label: string }[] = [
  { key: "24h", label: "24 hours" },
  { key: "7d", label: "7 days" },
  { key: "30d", label: "30 days" },
];

// The three questions the same table answers, in the order they get asked
// during an incident: what is slow, what is busy — which decides whether slow
// matters — and what is failing, which a latency chart never shows at all,
// because a request that fails fast looks fast.
const SORTS: { key: TransactionSort; label: string }[] = [
  { key: "p95", label: "Slowest" },
  { key: "count", label: "Busiest" },
  { key: "fail", label: "Failing" },
];

export function Performance({
  projectID,
  transaction,
  onSignedOut,
}: {
  projectID: number;
  transaction?: string | undefined;
  onSignedOut: () => void;
}) {
  const [range, setRange] = useState<DashboardRange>("24h");
  const [sort, setSort] = useState<TransactionSort>("p95");
  const [list, setList] = useState<TransactionList | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let current = true;
    setLoading(true);
    (async () => {
      try {
        const loaded = await api.listTransactions(projectID, range, sort);
        if (!current) return;
        setList(loaded);
        setError("");
      } catch (err) {
        if (!current) return;
        if (err instanceof ApiError && err.isUnauthorized) {
          onSignedOut();
          return;
        }
        setError(
          err instanceof Error ? err.message : "could not load the transactions",
        );
      } finally {
        if (current) setLoading(false);
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, range, sort, onSignedOut]);

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <Header onSignedOut={onSignedOut}>
        <Link to={projectsPath} className="font-normal text-muted hover:text-accent">
          Projects
        </Link>
        <span className="px-2 text-muted">/</span>
        Performance
      </Header>

      <ProjectNav projectID={projectID} current="performance" />

      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <Choices
          label="Range"
          options={RANGES}
          current={range}
          onChoose={setRange}
        />
        <Choices label="Sort" options={SORTS} current={sort} onChoose={setSort} />
      </div>

      <Notice>{error}</Notice>

      {loading && !list ? (
        <p className="text-sm text-muted">loading…</p>
      ) : !list || list.transactions.length === 0 ? (
        <Empty />
      ) : (
        <>
          <SamplingLine list={list} />
          <Table
            rows={list.transactions}
            projectID={projectID}
            selected={transaction}
          />
        </>
      )}

      {transaction ? (
        <Detail
          projectID={projectID}
          transaction={transaction}
          range={range}
          onSignedOut={onSignedOut}
        />
      ) : null}
    </div>
  );
}

// Buttons rather than links, like the dashboard's range: these change what one
// screen is showing, and putting them in the address would walk the back
// button through three table sortings before it left the page. The transaction
// underneath is the opposite — it is an address, and it is a link.
function Choices<T extends string>({
  label,
  options,
  current,
  onChoose,
}: {
  label: string;
  options: { key: T; label: string }[];
  current: T;
  onChoose: (value: T) => void;
}) {
  return (
    <div className="flex gap-1" role="group" aria-label={label}>
      {options.map((option) => {
        const active = option.key === current;
        return (
          <button
            key={option.key}
            type="button"
            aria-pressed={active}
            onClick={() => onChoose(option.key)}
            className={`rounded-md px-2.5 py-1 text-sm transition ${
              active
                ? "bg-accent/10 font-medium text-accent"
                : "text-muted hover:text-ink"
            }`}
          >
            {option.label}
          </button>
        );
      })}
    </div>
  );
}

function Empty() {
  return (
    <p className="text-sm text-muted">
      No transactions in this range. Tracing is off until a project asks for it:
      switch on the <code className="font-mono">transaction</code> category in
      Settings and give the SDK a{" "}
      <code className="font-mono">tracesSampleRate</code>. The aggregates are
      written for everything that arrives — the sample rate only decides how
      many whole waterfalls are kept.
    </p>
  );
}

function SamplingLine({ list }: { list: TransactionList }) {
  const { sampling } = list;
  const failShare = list.total > 0 ? (list.total_failed / list.total) * 100 : 0;
  return (
    <p className="text-xs text-muted">
      <span className="tabular-nums">{list.total.toLocaleString()}</span>{" "}
      transactions ·{" "}
      <span className="tabular-nums">{list.total_failed.toLocaleString()}</span>{" "}
      failed ({failShare.toFixed(1)}%) ·{" "}
      {/* The effective rate is observed rather than assumed: it includes the
          SDK's own sampling, and any stretch during which the server knob was
          set differently. */}
      <span className="tabular-nums">
        {sampling.stored.toLocaleString()}
      </span>{" "}
      waterfalls kept, {formatRate(sampling.effective_rate)} of{" "}
      <span className="tabular-nums">
        {sampling.received.toLocaleString()}
      </span>{" "}
      received
    </p>
  );
}

function Table({
  rows,
  projectID,
  selected,
}: {
  rows: TransactionSummary[];
  projectID: number;
  selected?: string | undefined;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <caption className="sr-only">
          Transactions, with the percentiles computed over the chosen range
        </caption>
        <thead>
          <tr className="border-b border-line text-left text-xs text-muted">
            <th scope="col" className="py-2 pr-3 font-normal">
              Transaction
            </th>
            <Numeric header>p50</Numeric>
            <Numeric header>p95</Numeric>
            <Numeric header>p99</Numeric>
            <Numeric header>Count</Numeric>
            <Numeric header>Fail</Numeric>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => {
            const open = row.transaction === selected;
            return (
              <tr
                key={row.transaction}
                className={`border-b border-line ${open ? "bg-line/30" : ""}`}
              >
                <th scope="row" className="min-w-0 max-w-xs py-2 pr-3 font-normal">
                  <Link
                    to={performancePath(projectID, row.transaction)}
                    className="block truncate font-mono hover:text-accent"
                    current={open}
                    label={`Open the history of ${row.transaction}`}
                  >
                    {row.transaction}
                  </Link>
                </th>
                <Numeric>{formatMS(row.p50_ms)}</Numeric>
                <Numeric>{formatMS(row.p95_ms)}</Numeric>
                <Numeric>{formatMS(row.p99_ms)}</Numeric>
                <Numeric>{row.count.toLocaleString()}</Numeric>
                {/* The share and not just the count: forty failures out of
                    forty is an outage and forty out of four million is
                    background noise, and the two look identical in one
                    column. */}
                <Numeric emphasis={row.failed > 0}>
                  {row.failed > 0
                    ? `${(row.fail_rate * 100).toFixed(1)}%`
                    : "—"}
                </Numeric>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function Numeric({
  children,
  header = false,
  emphasis = false,
}: {
  children: ReactNode;
  header?: boolean;
  emphasis?: boolean;
}) {
  const className = `py-2 pl-3 text-right tabular-nums ${
    emphasis ? "text-red-600" : ""
  }`;
  if (header) {
    return (
      <th scope="col" className={`${className} font-normal`}>
        {children}
      </th>
    );
  }
  return <td className={className}>{children}</td>;
}

// The history of one transaction, loaded when a row is opened.
//
// A second request rather than a field on the table: it is a different
// resolution over a different shape, and forty rows' worth of series would be
// forty sketch merges for the thirty-nine nobody clicked.
function Detail({
  projectID,
  transaction,
  range,
  onSignedOut,
}: {
  projectID: number;
  transaction: string;
  range: DashboardRange;
  onSignedOut: () => void;
}) {
  const [series, setSeries] = useState<TransactionSeries | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  // Minutes for a day, hours for anything longer. The two answer different
  // questions — "what happened during the incident" and "did the fix hold" —
  // and only minutes of the last 48 hours are stored, so asking for them over
  // thirty days would be asking for buckets that were downsampled away
  // (ADR 021).
  const resolution = range === "24h" ? "minute" : "hour";

  useEffect(() => {
    let current = true;
    setLoading(true);
    (async () => {
      try {
        const loaded = await api.transactionSeries(
          projectID,
          transaction,
          range,
          resolution,
        );
        if (!current) return;
        setSeries(loaded);
        setError("");
      } catch (err) {
        if (!current) return;
        if (err instanceof ApiError && err.isUnauthorized) {
          onSignedOut();
          return;
        }
        setError(err instanceof Error ? err.message : "could not load the series");
      } finally {
        if (current) setLoading(false);
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, transaction, range, resolution, onSignedOut]);

  return (
    <section className="space-y-3 border-t border-line pt-4">
      <div className="flex items-baseline justify-between gap-3">
        <h2 className="min-w-0 truncate font-mono text-sm font-medium">
          {transaction}
        </h2>
        <button
          type="button"
          className="shrink-0 text-xs text-muted hover:text-accent"
          onClick={() => navigate(performancePath(projectID))}
        >
          close
        </button>
      </div>

      <Notice>{error}</Notice>

      {loading && !series ? (
        <p className="text-sm text-muted">loading…</p>
      ) : !series ? null : (
        <>
          <LatencyLines points={series.series} resolution={series.resolution} />
          <dl className="grid grid-cols-3 gap-3 text-xs sm:grid-cols-6">
            <Figure label="p50" value={formatMS(series.summary.p50_ms)} />
            <Figure label="p95" value={formatMS(series.summary.p95_ms)} />
            <Figure label="p99" value={formatMS(series.summary.p99_ms)} />
            <Figure label="mean" value={formatMS(series.summary.mean_ms)} />
            <Figure label="max" value={formatMS(series.summary.max_ms)} />
            <Figure label="count" value={series.total.toLocaleString()} />
          </dl>
          {/* Said in words underneath, because the chart says it in pixels
              and a reader who cannot see the pixels gets the same answer. */}
          <p className="text-xs text-muted">
            Over this range, half the requests finished within{" "}
            {formatMS(series.summary.p50_ms)} and 95% within{" "}
            {formatMS(series.summary.p95_ms)}. Merged from the stored sketches
            when you asked — never a saved percentile.
          </p>
          <Examples
            projectID={projectID}
            traces={series.examples}
            sampled={series.summary.sampled}
          />
        </>
      )}
    </section>
  );
}

function Figure({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border border-line p-2">
      <dt className="text-muted">{label}</dt>
      <dd className="mt-0.5 font-medium tabular-nums">{value}</dd>
    </div>
  );
}

function Examples({
  projectID,
  traces,
  sampled,
}: {
  projectID: number;
  traces: Trace[];
  sampled: number;
}) {
  if (traces.length === 0) {
    return (
      <p className="text-xs text-muted">
        {sampled === 0
          ? "No waterfall was kept for this transaction in this range — the aggregates above still count every request. Raise traces_sample_rate in Settings to keep some."
          : "No stored waterfall left for this range; raw traces are kept for less time than the aggregates."}
      </p>
    );
  }
  return (
    <div className="space-y-1">
      <h3 className="text-xs font-medium text-muted">Slowest kept traces</h3>
      <ul>
        {traces.map((trace) => (
          <li key={trace.trace_id} className="border-b border-line last:border-0">
            <Link
              to={tracePath(projectID, trace.trace_id)}
              className="-mx-2 flex items-baseline justify-between gap-3 rounded px-2 py-1.5 text-xs hover:bg-line/30"
              label={`Open the waterfall of trace ${trace.trace_id}`}
            >
              <span className="truncate font-mono">{trace.trace_id}</span>
              <span className="flex shrink-0 items-baseline gap-3 text-muted">
                <Ago at={trace.timestamp} />
                <span className="tabular-nums">{formatMS(trace.duration_ms)}</span>
              </span>
            </Link>
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * formatMS reads a duration the way somebody says it out loud.
 *
 * Milliseconds under a second, seconds above — and never more precision than
 * the sketch has: it stores 1% relative error, so `123.4567 ms` would be four
 * digits of confidence this number does not have (ADR 020).
 */
export function formatMS(ms: number): string {
  if (!Number.isFinite(ms)) return "—";
  if (ms >= 1000) return `${(ms / 1000).toFixed(ms >= 10_000 ? 0 : 1)} s`;
  if (ms >= 10) return `${Math.round(ms)} ms`;
  return `${ms.toFixed(1)} ms`;
}

/** A share, written as a percentage nobody has to squint at. */
function formatRate(rate: number): string {
  if (rate >= 0.1) return `${Math.round(rate * 100)}%`;
  if (rate <= 0) return "0%";
  return `${(rate * 100).toFixed(1)}%`;
}
