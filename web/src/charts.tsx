// The panel's charts, drawn by hand in SVG.
//
// No charting library. Not for the bundle size, though it is a real one for a
// panel that ships inside a 30 MB binary — but because everything drawn here
// is a bar or a line over a series the server already shaped: the hours come
// back complete, gaps included, and the totals come back computed. What a
// library would add is a layout engine for axes nobody is reading and a
// second theming system to keep in step with the first.
//
// Two rules run through all of it:
//
//   * **Colour is never the only carrier.** Every severity is written out in
//     the legend with its count, every bar has a title a pointer reveals and a
//     screen reader announces, and the summary under each chart says in words
//     what the shape says in pixels. A red column means nothing to a reader
//     who cannot see it is red, and nothing at all in a screenshot pasted
//     into a ticket.
//
//   * **The palette comes from CSS custom properties, not from constants
//     here.** They are defined once in index.css and overridden for a dark
//     reader in the same place — deliberately outside Tailwind's `@theme`,
//     which hoists every block it finds into one unconditional `:root` rule
//     and once shipped the dark ramp to everybody (bug #5, 24-aug).

import type { ReactNode } from "react";
import type { Level } from "./api";

/**
 * LEVELS is severity order, worst first.
 *
 * Fixed rather than whatever the server's map iterated, so a stack does not
 * reshuffle between two refreshes of the same chart and two charts side by
 * side are comparable.
 */
export const LEVELS: Level[] = ["fatal", "error", "warning", "info", "debug"];

const levelColour: Record<Level, string> = {
  fatal: "var(--chart-fatal)",
  error: "var(--chart-error)",
  warning: "var(--chart-warning)",
  info: "var(--chart-info)",
  debug: "var(--chart-debug)",
};

/** A point of the project series: one hour, its total and its severities. */
export interface HourPoint {
  hour: string;
  count: number;
  by_level: Record<string, number>;
}

/**
 * hourLabel renders a bucket the way a local reader thinks about time.
 *
 * The buckets are UTC, because storage that is anything else is storage that
 * changes meaning twice a year. What a person reads them as is their own
 * clock, so the conversion happens here, at the last possible moment.
 */
export function hourLabel(hour: string): string {
  const parsed = new Date(`${hour}:00:00Z`);
  if (Number.isNaN(parsed.getTime())) return hour;
  return parsed.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
  });
}

const BAR_VIEW_WIDTH = 1000;
const BAR_VIEW_HEIGHT = 120;

/**
 * HourlyBars is the dashboard's main chart: one stacked column per hour.
 *
 * Stacked rather than grouped because the first question is "how much is
 * breaking" and the second is "what kind" — a stack answers both in that
 * order, and grouped bars answer neither at a glance once a range is thirty
 * days long.
 *
 * The columns are drawn in a fixed viewBox stretched to whatever width the
 * page gives it. Rectangles survive that; the line chart below does not, and
 * says how it deals with it.
 */
export function HourlyBars({
  points,
  levels,
}: {
  points: HourPoint[];
  levels: Record<string, number>;
}) {
  const total = points.reduce((sum, point) => sum + point.count, 0);
  if (points.length === 0) {
    return <Empty>No hours in this range.</Empty>;
  }

  const tallest = Math.max(...points.map((point) => point.count), 1);
  const step = BAR_VIEW_WIDTH / points.length;
  // A visible gap between columns, but never more than a fifth of one: at
  // thirty days the columns are already hairlines and a fixed gap would eat
  // them entirely.
  const gap = Math.min(step * 0.2, 2);
  const width = Math.max(step - gap, 0.5);

  const present = LEVELS.filter((level) => (levels[level] ?? 0) > 0);
  const busiest = points.reduce((worst, point) =>
    point.count > worst.count ? point : worst,
  );

  return (
    <figure className="space-y-2">
      <svg
        viewBox={`0 0 ${BAR_VIEW_WIDTH} ${BAR_VIEW_HEIGHT}`}
        preserveAspectRatio="none"
        className="h-32 w-full"
        role="img"
        aria-label={`${total.toLocaleString()} events over ${points.length} hours, busiest at ${hourLabel(busiest.hour)} with ${busiest.count.toLocaleString()}`}
      >
        {points.map((point, index) => {
          let consumed = 0;
          return (
            <g key={point.hour}>
              {/* The title is on the whole column, not on each segment: a
                  pointer landing between two one-pixel slices would otherwise
                  show whichever it happened to hit. */}
              <title>{`${hourLabel(point.hour)} — ${point.count.toLocaleString()} ${point.count === 1 ? "event" : "events"}${describeLevels(point.by_level)}`}</title>
              {/* An invisible full-height rectangle so the whole column is a
                  hover target, including the empty space above a short bar. */}
              <rect
                x={index * step}
                y={0}
                width={step}
                height={BAR_VIEW_HEIGHT}
                fill="transparent"
              />
              {LEVELS.map((level) => {
                const count = point.by_level[level] ?? 0;
                if (count === 0) return null;
                const height = (count / tallest) * BAR_VIEW_HEIGHT;
                const y = BAR_VIEW_HEIGHT - consumed - height;
                consumed += height;
                return (
                  <rect
                    key={level}
                    x={index * step + gap / 2}
                    y={y}
                    width={width}
                    // A bucket that rounds to nothing still happened, and a
                    // chart that draws it as nothing is a chart claiming a
                    // quiet hour.
                    height={Math.max(height, 0.75)}
                    fill={levelColour[level]}
                  />
                );
              })}
            </g>
          );
        })}
      </svg>

      <div className="flex items-baseline justify-between gap-3 text-xs text-muted">
        <span>{hourLabel(points[0]!.hour)}</span>
        <span className="tabular-nums">peak {tallest.toLocaleString()}/h</span>
        <span>{hourLabel(points[points.length - 1]!.hour)}</span>
      </div>

      <figcaption className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
        {present.length === 0 ? (
          <span className="text-muted">Nothing in this range.</span>
        ) : (
          present.map((level) => (
            <span key={level} className="inline-flex items-center gap-1.5">
              <span
                aria-hidden="true"
                className="inline-block h-2 w-2 rounded-sm"
                style={{ background: levelColour[level] }}
              />
              {/* The word, always. The swatch beside it is a convenience for
                  readers who can use it, never the thing carrying the fact. */}
              <span>{level}</span>
              <span className="text-muted tabular-nums">
                {(levels[level] ?? 0).toLocaleString()}
              </span>
            </span>
          ))
        )}
      </figcaption>
    </figure>
  );
}

function describeLevels(byLevel: Record<string, number>): string {
  const parts = LEVELS.filter((level) => (byLevel[level] ?? 0) > 0).map(
    (level) => `${level} ${byLevel[level]}`,
  );
  return parts.length > 1 ? ` (${parts.join(", ")})` : "";
}

const SPARK_VIEW_WIDTH = 100;
const SPARK_VIEW_HEIGHT = 24;

/**
 * Sparkline is one issue's last day, small enough to sit on a list row.
 *
 * A line rather than bars at this size: twenty-four bars in eighty pixels is a
 * texture, and what somebody is reading off a row is a direction — climbing,
 * stopped, spiky — which is exactly what a line carries and bars do not.
 *
 * `preserveAspectRatio="none"` stretches the box to the width available, which
 * would also stretch the stroke. `vectorEffect="non-scaling-stroke"` is what
 * keeps the line one pixel wide however wide the row gets.
 */
export function Sparkline({
  counts,
  label,
  className = "h-6 w-20",
}: {
  counts: number[];
  label: string;
  className?: string;
}) {
  if (counts.length === 0) return null;
  const total = counts.reduce((sum, count) => sum + count, 0);
  const tallest = Math.max(...counts, 1);
  const step = counts.length > 1 ? SPARK_VIEW_WIDTH / (counts.length - 1) : 0;

  const points = counts
    .map((count, index) => {
      const x = index * step;
      // One unit of headroom top and bottom so a flat maximum is a visible
      // line rather than a clipped edge.
      const y =
        SPARK_VIEW_HEIGHT - 1 - (count / tallest) * (SPARK_VIEW_HEIGHT - 2);
      return `${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(" ");

  return (
    <svg
      viewBox={`0 0 ${SPARK_VIEW_WIDTH} ${SPARK_VIEW_HEIGHT}`}
      preserveAspectRatio="none"
      className={className}
      role="img"
      aria-label={`${label}: ${total.toLocaleString()} in the last ${counts.length} hours`}
    >
      <title>{`${total.toLocaleString()} in the last ${counts.length} hours, peak ${tallest.toLocaleString()}/h`}</title>
      <polyline
        points={points}
        fill="none"
        stroke="var(--chart-line)"
        strokeWidth="1.5"
        strokeLinejoin="round"
        strokeLinecap="round"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}

/** One row of a breakdown: a value and how many events carried it. */
export interface RankedItem {
  value: string;
  count: number;
}

/**
 * RankedBars is a breakdown — by release, by environment — as horizontal bars.
 *
 * Horizontal because the labels are release strings and environment names, and
 * a vertical chart puts those on their side or truncates them, which for
 * `myapp@2.3.1-rc.4` means the part that distinguishes it is the part that
 * goes. The count is written next to every bar, so the chart is readable with
 * the bars ignored entirely.
 */
export function RankedBars({
  items,
  empty,
  onSelect,
}: {
  items: RankedItem[];
  empty: string;
  onSelect?: (value: string) => void;
}) {
  if (items.length === 0) return <Empty>{empty}</Empty>;
  const total = items.reduce((sum, item) => sum + item.count, 0);
  const largest = Math.max(...items.map((item) => item.count), 1);

  return (
    <ul className="space-y-1.5">
      {items.map((item) => {
        const share = total > 0 ? (item.count / total) * 100 : 0;
        const label = (
          <>
            <span className="flex items-baseline justify-between gap-3 text-xs">
              <span className="truncate font-mono" title={item.value}>
                {item.value || "(none)"}
              </span>
              <span className="shrink-0 text-muted tabular-nums">
                {item.count.toLocaleString()} · {share.toFixed(0)}%
              </span>
            </span>
            <svg
              viewBox="0 0 100 4"
              preserveAspectRatio="none"
              className="mt-1 h-1 w-full"
              aria-hidden="true"
            >
              <rect x="0" y="0" width="100" height="4" fill="var(--chart-track)" rx="1" />
              <rect
                x="0"
                y="0"
                width={Math.max((item.count / largest) * 100, 0.5)}
                height="4"
                fill="var(--chart-bar)"
                rx="1"
              />
            </svg>
          </>
        );

        return (
          <li key={item.value}>
            {onSelect ? (
              <button
                type="button"
                className="-mx-2 block w-full rounded px-2 py-1 text-left hover:bg-line/30"
                onClick={() => onSelect(item.value)}
                // Says what pressing it does, because the visible text is a
                // release string and a number, neither of which is a verb.
                aria-label={`Filter issues by ${item.value}`}
              >
                {label}
              </button>
            ) : (
              <div className="-mx-2 px-2 py-1">{label}</div>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function Empty({ children }: { children: ReactNode }) {
  return <p className="py-4 text-sm text-muted">{children}</p>;
}

const LINE_VIEW_WIDTH = 1000;
const LINE_VIEW_HEIGHT = 140;

/** One bucket of a latency series, as the tracing endpoints return it. */
export interface LatencyPoint {
  bucket: string;
  count: number;
  failed: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
}

/**
 * bucketLabel renders a tracing bucket in the reader's own clock.
 *
 * Two shapes arrive — `YYYY-MM-DDTHH:MM` and `YYYY-MM-DDTHH` — because the
 * series comes at two resolutions, and the label has to say minutes only when
 * there are minutes to say. A bucket labelled "14:00" for a whole hour and for
 * one minute of it is the same label for two different claims.
 */
export function bucketLabel(bucket: string): string {
  const minutes = bucket.length > 13;
  const parsed = new Date(minutes ? `${bucket}:00Z` : `${bucket}:00:00Z`);
  if (Number.isNaN(parsed.getTime())) return bucket;
  return parsed.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    ...(minutes ? { minute: "2-digit" } : {}),
  });
}

/**
 * LatencyLines draws p50, p95 and p99 over the same buckets.
 *
 * Three lines and not three charts: the question is the distance between them.
 * A p95 that climbs while p50 stays flat is a tail — a few requests suffering,
 * most of them fine — and a p50 that climbs with it is everything getting
 * slower. Those are different incidents with different causes, and stacked
 * charts with independent vertical scales hide exactly that difference.
 *
 * One shared scale for that reason, and it is p99's: normalising each line to
 * its own maximum would draw a flat p50 and a flat p99 as the same line.
 *
 * Empty buckets are gaps, not zeros. The server returns every bucket in the
 * range including the ones nothing arrived in, and drawing those at zero would
 * paint a quiet night as the fastest the service has ever been.
 */
export function LatencyLines({
  points,
  resolution,
}: {
  points: LatencyPoint[];
  resolution: "minute" | "hour";
}) {
  const withTraffic = points.filter((point) => point.count > 0);
  if (withTraffic.length === 0) {
    return <Empty>Nothing arrived in this range.</Empty>;
  }

  const ceiling = Math.max(...withTraffic.map((point) => point.p99_ms), 1);
  const step = points.length > 1 ? LINE_VIEW_WIDTH / (points.length - 1) : 0;
  const y = (value: number) =>
    LINE_VIEW_HEIGHT - 2 - (value / ceiling) * (LINE_VIEW_HEIGHT - 4);

  // A series is a list of runs: consecutive buckets that had traffic. Each run
  // is its own polyline, which is what makes a gap look like a gap instead of
  // a line dropping to the floor and back.
  const runs = (pick: (point: LatencyPoint) => number): string[] => {
    const drawn: string[] = [];
    let run: string[] = [];
    points.forEach((point, index) => {
      if (point.count === 0) {
        if (run.length > 0) drawn.push(run.join(" "));
        run = [];
        return;
      }
      run.push(`${(index * step).toFixed(2)},${y(pick(point)).toFixed(2)}`);
    });
    if (run.length > 0) drawn.push(run.join(" "));
    return drawn;
  };

  const series: {
    key: string;
    label: string;
    colour: string;
    dash?: string;
    pick: (point: LatencyPoint) => number;
  }[] = [
    { key: "p99", label: "p99", colour: "var(--chart-fatal)", dash: "6 4", pick: (p) => p.p99_ms },
    { key: "p95", label: "p95", colour: "var(--chart-error)", pick: (p) => p.p95_ms },
    { key: "p50", label: "p50", colour: "var(--chart-info)", dash: "2 3", pick: (p) => p.p50_ms },
  ];

  const slowest = withTraffic.reduce((worst, point) =>
    point.p95_ms > worst.p95_ms ? point : worst,
  );
  const busiest = withTraffic.reduce((most, point) =>
    point.count > most.count ? point : most,
  );

  return (
    <figure className="space-y-2">
      <svg
        viewBox={`0 0 ${LINE_VIEW_WIDTH} ${LINE_VIEW_HEIGHT}`}
        preserveAspectRatio="none"
        className="h-36 w-full"
        role="img"
        aria-label={`Latency over ${points.length} ${resolution} buckets. Worst p95 ${Math.round(slowest.p95_ms)} milliseconds at ${bucketLabel(slowest.bucket)}; busiest ${busiest.count.toLocaleString()} requests at ${bucketLabel(busiest.bucket)}`}
      >
        {series.map((line) =>
          runs(line.pick).map((run, index) => (
            <polyline
              key={`${line.key}-${index}`}
              points={run}
              fill="none"
              stroke={line.colour}
              strokeWidth="1.5"
              strokeDasharray={line.dash}
              strokeLinejoin="round"
              strokeLinecap="round"
              vectorEffect="non-scaling-stroke"
            />
          )),
        )}
      </svg>
      {/* The legend carries the value as well as the colour, and the dash
          pattern differs per line: a chart whose only distinction is hue says
          nothing to a reader who cannot see hue, and nothing at all in a
          screenshot pasted into a ticket. */}
      <figcaption className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
        {series.map((line) => (
          <span key={line.key} className="inline-flex items-baseline gap-1.5">
            <svg viewBox="0 0 16 4" className="h-1 w-4" aria-hidden="true">
              <line
                x1="0"
                y1="2"
                x2="16"
                y2="2"
                stroke={line.colour}
                strokeWidth="2"
                strokeDasharray={line.dash}
              />
            </svg>
            {line.label}
          </span>
        ))}
        <span>
          peak p99 {Math.round(ceiling).toLocaleString()} ms · {points.length}{" "}
          {resolution === "minute" ? "minutes" : "hours"}
        </span>
      </figcaption>
    </figure>
  );
}
