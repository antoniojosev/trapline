/**
 * Monitors: the two families on one screen.
 *
 * One screen and not two, because a cron monitor and an uptime monitor are the
 * same concept watched from opposite sides — one waits to be told, the other
 * goes and asks (ADR 037). An operator opening this asks "what is this
 * installation watching, and is any of it unhappy", and the answer must not
 * depend on which of the two words they remembered.
 *
 * The public page's settings live at the bottom of the same screen for the
 * same reason: what the world sees is a property of these monitors, and a
 * separate screen for one checkbox and two text fields is a screen nobody
 * finds.
 */

import { useCallback, useEffect, useState } from "react";
import type {
  CronCheckIn,
  CronMonitor,
  UptimeDay,
  UptimeMonitor,
} from "./api";
import { api } from "./api";
import { Ago, Button, Field, Header, Notice, ProjectNav } from "./ui";

type Loaded = {
  crons: CronMonitor[];
  uptime: UptimeMonitor[];
  /** Ninety days per uptime monitor, keyed by monitor id. */
  history: Record<number, UptimeDay[]>;
  statusPageEnabled: boolean;
  projectSlug: string;
};

export function Monitors({
  projectID,
  onSignedOut,
}: {
  projectID: number;
  onSignedOut: () => void;
}) {
  const [state, setState] = useState<Loaded | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const [crons, uptime, config, project] = await Promise.all([
        api.listCronMonitors(projectID),
        api.listUptimeMonitors(projectID),
        api.getProjectConfig(projectID),
        api.getProject(projectID),
      ]);
      // The ninety-day history is one request per uptime monitor, and it is
      // ninety rows each — the roll-up exists so this screen never reads a
      // check (ADR 001, ADR 017).
      const history: Record<number, UptimeDay[]> = {};
      await Promise.all(
        uptime.monitors.map(async (monitor) => {
          const { days } = await api.uptimeDaily(monitor.id);
          history[monitor.id] = days;
        }),
      );
      setState({
        crons: crons.monitors,
        uptime: uptime.monitors,
        history,
        statusPageEnabled: config.status_page.enabled,
        projectSlug: project.slug,
      });
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "could not load monitors");
    }
  }, [projectID]);

  useEffect(() => {
    void load();
  }, [load]);

  async function run(action: () => Promise<unknown>) {
    setBusy(true);
    try {
      await action();
      await load();
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "that did not work");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="mx-auto max-w-5xl space-y-6 p-6">
      <Header onSignedOut={onSignedOut}>Monitors</Header>
      <ProjectNav projectID={projectID} current="monitors" />
      <Notice>{error}</Notice>

      {state === null ? (
        <p className="text-sm text-muted">loading…</p>
      ) : (
        <>
          <UptimeSection
            projectID={projectID}
            monitors={state.uptime}
            history={state.history}
            busy={busy}
            run={run}
          />
          <CronSection
            projectID={projectID}
            monitors={state.crons}
            busy={busy}
            run={run}
          />
          <StatusPageSection
            projectID={projectID}
            slug={state.projectSlug}
            enabled={state.statusPageEnabled}
            publicCount={state.uptime.filter((m) => m.public).length}
            busy={busy}
            run={run}
          />
        </>
      )}
    </div>
  );
}

/* ------------------------------------------------------------------ uptime */

function UptimeSection({
  projectID,
  monitors,
  history,
  busy,
  run,
}: {
  projectID: number;
  monitors: UptimeMonitor[];
  history: Record<number, UptimeDay[]>;
  busy: boolean;
  run: (action: () => Promise<unknown>) => Promise<void>;
}) {
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [isPublic, setIsPublic] = useState(false);

  return (
    <section className="space-y-3">
      <h2 className="text-base font-semibold">Uptime</h2>
      <p className="text-sm text-muted">
        A URL this server fetches on a schedule. It goes down after two
        consecutive failures, so a single dropped packet is not an incident.
      </p>

      {monitors.length === 0 ? (
        <p className="text-sm text-muted">Nothing is being checked yet.</p>
      ) : (
        <ul className="space-y-3">
          {monitors.map((monitor) => (
            <li
              key={monitor.id}
              className="rounded-md border border-line p-3 space-y-2"
            >
              <div className="flex flex-wrap items-baseline gap-2">
                <StatusDot status={monitor.status} />
                <span className="font-medium">{monitor.name}</span>
                {monitor.public && (
                  <span className="rounded bg-accent/10 px-1.5 py-0.5 text-xs text-accent">
                    on the status page
                  </span>
                )}
                {!monitor.enabled && (
                  <span className="text-xs text-muted">paused</span>
                )}
                <span className="ml-auto text-xs text-muted">
                  {monitor.method} every {monitor.interval_s}s
                  {monitor.last_checked_at && (
                    <>
                      {" · last checked "}
                      <Ago at={monitor.last_checked_at} />
                    </>
                  )}
                </span>
              </div>
              <p className="break-all font-mono text-xs text-muted">
                {monitor.url}
              </p>
              <NinetyDayBar days={history[monitor.id] ?? []} />
              <div className="flex gap-2">
                <Button
                  variant="quiet"
                  disabled={busy}
                  onClick={() =>
                    void run(() =>
                      api.setUptimeMonitorEnabled(monitor.id, !monitor.enabled),
                    )
                  }
                >
                  {monitor.enabled ? "Pause" : "Resume"}
                </Button>
                <Button
                  variant="quiet"
                  disabled={busy}
                  onClick={() =>
                    void run(() => api.deleteUptimeMonitor(monitor.id))
                  }
                >
                  Delete
                </Button>
                <UptimeResults monitorID={monitor.id} />
              </div>
            </li>
          ))}
        </ul>
      )}

      <form
        className="flex flex-wrap items-end gap-3 rounded-md border border-line p-3"
        onSubmit={(event) => {
          event.preventDefault();
          void run(async () => {
            await api.createUptimeMonitor(projectID, {
              name,
              url,
              public: isPublic,
            });
            setName("");
            setUrl("");
            setIsPublic(false);
          });
        }}
      >
        <Field
          label="Name"
          value={name}
          onChange={(event) => setName(event.target.value)}
          placeholder="checkout"
          required
        />
        <Field
          label="URL"
          value={url}
          onChange={(event) => setUrl(event.target.value)}
          placeholder="https://example.com/health"
          required
        />
        <label className="flex items-center gap-2 pb-2 text-sm">
          <input
            type="checkbox"
            checked={isPublic}
            onChange={(event) => setIsPublic(event.target.checked)}
          />
          Show on the public status page
        </label>
        <Button type="submit" disabled={busy}>
          Add
        </Button>
      </form>
    </section>
  );
}

/**
 * NinetyDayBar is the same picture the public page draws, from the same rows.
 *
 * A day the monitor did not exist for is grey and not green: a bar that
 * claimed three months of uptime for a monitor added this morning would be
 * the panel telling its operator something the data does not say.
 */
function NinetyDayBar({ days }: { days: UptimeDay[] }) {
  const byDay = new Map(days.map((day) => [day.day, day]));
  const today = new Date();
  const cells = [];
  for (let offset = 89; offset >= 0; offset--) {
    const date = new Date(today);
    date.setUTCDate(date.getUTCDate() - offset);
    const key = date.toISOString().slice(0, 10);
    const day = byDay.get(key);
    let tone = "bg-line";
    let title = `${key}: no data`;
    if (day && day.checks > 0) {
      title = `${key}: ${day.uptime.toFixed(2)} % of ${day.checks} checks`;
      if (day.failures === 0) tone = "bg-emerald-500";
      else if (day.failures >= day.checks) tone = "bg-red-500";
      else tone = "bg-amber-500";
    }
    cells.push(
      <span
        key={key}
        title={title}
        className={`h-6 min-w-[3px] flex-1 rounded-[2px] ${tone}`}
      />,
    );
  }
  return (
    <div>
      <div className="flex gap-[2px] overflow-x-auto">{cells}</div>
      <div className="flex justify-between pt-1 text-[11px] text-muted">
        <span>90 days ago</span>
        <span>today</span>
      </div>
    </div>
  );
}

/**
 * UptimeResults is the recent checks, fetched only when asked for.
 *
 * Not loaded with the screen: it is the detail somebody opens when a monitor
 * is unhappy, and loading it for every monitor on every visit would be a query
 * per monitor for something nobody is looking at.
 */
function UptimeResults({ monitorID }: { monitorID: number }) {
  const [rows, setRows] = useState<null | Awaited<
    ReturnType<typeof api.listUptimeResults>
  >["results"]>(null);
  const [open, setOpen] = useState(false);

  async function toggle() {
    if (open) {
      setOpen(false);
      return;
    }
    setOpen(true);
    if (rows === null) {
      const { results } = await api.listUptimeResults(monitorID, 20);
      setRows(results);
    }
  }

  return (
    <>
      <Button variant="quiet" onClick={() => void toggle()}>
        {open ? "Hide checks" : "Recent checks"}
      </Button>
      {open && (
        <ul className="w-full space-y-1 pt-2 text-xs">
          {(rows ?? []).map((result) => (
            <li key={result.at} className="flex gap-3">
              <span className={result.ok ? "text-emerald-600" : "text-red-600"}>
                {result.ok ? "ok" : "failed"}
              </span>
              <Ago at={result.at} />
              <span className="text-muted">
                {result.status_code || "—"} · {result.latency_ms} ms
              </span>
              {result.error && (
                <span className="truncate text-red-600">{result.error}</span>
              )}
            </li>
          ))}
          {rows !== null && rows.length === 0 && (
            <li className="text-muted">nothing yet</li>
          )}
        </ul>
      )}
    </>
  );
}

/* -------------------------------------------------------------------- cron */

function CronSection({
  projectID,
  monitors,
  busy,
  run,
}: {
  projectID: number;
  monitors: CronMonitor[];
  busy: boolean;
  run: (action: () => Promise<unknown>) => Promise<void>;
}) {
  const [slug, setSlug] = useState("");
  const [schedule, setSchedule] = useState("");
  const [timezone, setTimezone] = useState("UTC");

  return (
    <section className="space-y-3">
      <h2 className="text-base font-semibold">Cron</h2>
      <p className="text-sm text-muted">
        A job that is supposed to report in. Add{" "}
        <code className="font-mono">{"&& curl -fsS <ping url>"}</code> to the
        end of its crontab line, and this notices when it stops.
      </p>

      {monitors.length === 0 ? (
        <p className="text-sm text-muted">No cron monitors yet.</p>
      ) : (
        <ul className="space-y-3">
          {monitors.map((monitor) => (
            <li
              key={monitor.id}
              className="space-y-2 rounded-md border border-line p-3"
            >
              <div className="flex flex-wrap items-baseline gap-2">
                <CronDot status={monitor.status} />
                <span className="font-medium">{monitor.slug}</span>
                <span className="font-mono text-xs text-muted">
                  {monitor.schedule} ({monitor.timezone})
                </span>
                <span className="ml-auto text-xs text-muted">
                  {monitor.last_checkin_at ? (
                    <>
                      last check-in <Ago at={monitor.last_checkin_at} />
                    </>
                  ) : (
                    "never checked in"
                  )}
                </span>
              </div>
              <p className="break-all font-mono text-xs text-muted">
                {monitor.ping_url}
              </p>
              <div className="flex gap-2">
                <CheckIns projectID={projectID} monitorID={monitor.id} />
                <Button
                  variant="quiet"
                  disabled={busy}
                  onClick={() =>
                    void run(() => api.deleteCronMonitor(projectID, monitor.id))
                  }
                >
                  Delete
                </Button>
              </div>
            </li>
          ))}
        </ul>
      )}

      <form
        className="flex flex-wrap items-end gap-3 rounded-md border border-line p-3"
        onSubmit={(event) => {
          event.preventDefault();
          void run(async () => {
            await api.createCronMonitor(projectID, {
              slug,
              schedule,
              timezone,
            });
            setSlug("");
            setSchedule("");
          });
        }}
      >
        <Field
          label="Name"
          value={slug}
          onChange={(event) => setSlug(event.target.value)}
          placeholder="nightly-backup"
          required
        />
        <Field
          label="Schedule"
          value={schedule}
          onChange={(event) => setSchedule(event.target.value)}
          placeholder="0 3 * * *"
          required
        />
        <Field
          label="Time zone"
          value={timezone}
          onChange={(event) => setTimezone(event.target.value)}
          placeholder="America/Caracas"
        />
        <Button type="submit" disabled={busy}>
          Add
        </Button>
      </form>
    </section>
  );
}

function CheckIns({
  projectID,
  monitorID,
}: {
  projectID: number;
  monitorID: number;
}) {
  const [rows, setRows] = useState<CronCheckIn[] | null>(null);
  const [open, setOpen] = useState(false);

  async function toggle() {
    if (open) {
      setOpen(false);
      return;
    }
    setOpen(true);
    if (rows === null) {
      const { checkins } = await api.listCheckIns(projectID, monitorID, 20);
      setRows(checkins);
    }
  }

  return (
    <>
      <Button variant="quiet" onClick={() => void toggle()}>
        {open ? "Hide runs" : "Recent runs"}
      </Button>
      {open && (
        <ul className="w-full space-y-1 pt-2 text-xs">
          {(rows ?? []).map((checkIn) => (
            <li key={checkIn.id} className="flex gap-3">
              <span
                className={
                  checkIn.status === "ok"
                    ? "text-emerald-600"
                    : checkIn.status === "error"
                      ? "text-red-600"
                      : "text-muted"
                }
              >
                {checkIn.status}
              </span>
              <Ago at={checkIn.started_at} />
              {checkIn.duration_ms > 0 && (
                <span className="text-muted">{checkIn.duration_ms} ms</span>
              )}
            </li>
          ))}
          {rows !== null && rows.length === 0 && (
            <li className="text-muted">nothing yet</li>
          )}
        </ul>
      )}
    </>
  );
}

/* ------------------------------------------------------------- status page */

function StatusPageSection({
  projectID,
  slug,
  enabled,
  publicCount,
  busy,
  run,
}: {
  projectID: number;
  slug: string;
  enabled: boolean;
  publicCount: number;
  busy: boolean;
  run: (action: () => Promise<unknown>) => Promise<void>;
}) {
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [loaded, setLoaded] = useState(false);
  const [saved, setSaved] = useState(false);
  // The checkbox moves when it is clicked and is corrected by the server's
  // answer, rather than staying put until a round trip finishes. A box that
  // does not move when clicked reads as broken — and a disabled one is worse,
  // because the next click lands on nothing at all.
  const [published, setPublished] = useState(enabled);
  useEffect(() => setPublished(enabled), [enabled]);

  useEffect(() => {
    void (async () => {
      const settings = await api.getStatusPageSettings();
      setTitle(settings.title);
      setDescription(settings.description);
      setLoaded(true);
    })();
  }, []);

  return (
    <section className="space-y-3 border-t border-line pt-6">
      <h2 className="text-base font-semibold">Public status page</h2>
      <p className="text-sm text-muted">
        A page anybody can read, with no login and no JavaScript, showing only
        the monitors marked public. The heading is shared by every project&apos;s
        page; which monitors appear is per monitor, and whether the page exists
        at all is per project.
      </p>

      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={published}
          onChange={(event) => {
            const next = event.target.checked;
            setPublished(next);
            void run(() =>
              api.setProjectConfig(projectID, {
                status_page: { enabled: next },
              }),
            );
          }}
        />
        Publish this project at{" "}
        <code className="font-mono">/status/{slug}</code>
      </label>

      {published && publicCount === 0 && (
        <p className="text-sm text-amber-600">
          The page is published and no monitor is marked public, so it says
          nothing is being reported on. Tick &ldquo;on the status page&rdquo; on
          a monitor above.
        </p>
      )}
      {published && (
        <p className="text-sm">
          <a
            className="text-accent underline"
            href={`/status/${slug}`}
            target="_blank"
            rel="noreferrer"
          >
            Open the page
          </a>{" "}
          <span className="text-muted">
            — showing {publicCount} monitor{publicCount === 1 ? "" : "s"}
          </span>
        </p>
      )}

      <form
        className="flex flex-wrap items-end gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          void run(async () => {
            await api.setStatusPageSettings(title, description);
            setSaved(true);
          });
        }}
      >
        <Field
          label="Title"
          value={title}
          disabled={!loaded}
          onChange={(event) => {
            setTitle(event.target.value);
            setSaved(false);
          }}
          placeholder="each page uses its project's name"
        />
        <Field
          label="Description"
          value={description}
          disabled={!loaded}
          onChange={(event) => {
            setDescription(event.target.value);
            setSaved(false);
          }}
          placeholder="optional"
        />
        <Button type="submit" disabled={busy || !loaded}>
          Save
        </Button>
        {saved && <span className="pb-2 text-sm text-muted">saved</span>}
      </form>
    </section>
  );
}

/* ------------------------------------------------------------------- bits */

function StatusDot({ status }: { status: UptimeMonitor["status"] }) {
  const tone =
    status === "up"
      ? "bg-emerald-500"
      : status === "down"
        ? "bg-red-500"
        : "bg-line";
  return (
    <span className="flex items-center gap-1.5 text-xs text-muted">
      <span className={`inline-block h-2 w-2 rounded-full ${tone}`} />
      {status}
    </span>
  );
}

function CronDot({ status }: { status: CronMonitor["status"] }) {
  const tone =
    status === "ok"
      ? "bg-emerald-500"
      : status === "unknown"
        ? "bg-line"
        : "bg-red-500";
  return (
    <span className="flex items-center gap-1.5 text-xs text-muted">
      <span className={`inline-block h-2 w-2 rounded-full ${tone}`} />
      {status}
    </span>
  );
}
