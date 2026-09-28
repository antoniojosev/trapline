// Alerting, configured from the panel: channels, rules, the weekly digest and
// the delivery log.
//
// One screen for the whole installation rather than one per project. A Slack
// workspace is not a property of an application, and two copies of the same
// channel under two projects is the arrangement that sends four identical
// messages during one incident. A rule chooses its scope instead.
//
// Two things on this page are easy to get quietly wrong, and both are the
// difference between an alerting system and the appearance of one:
//
//   - A secret goes in and never comes back. The encryption at rest of
//     ADR 015 buys nothing if the panel echoes a bot token into a listing, so
//     what is rendered is the redacted configuration the API returns, and the
//     form says plainly that a secret cannot be read back or edited.
//   - `failed` and `dead` are not the same colour, the same word or the same
//     sentence. A failed delivery is retried on its own; a dead one has spent
//     its ten attempts and will not be tried again unless somebody asks.
//     Showing them as one state is somebody waiting for a message that is
//     never coming.

import { useCallback, useEffect, useState } from "react";
import {
  ApiError,
  api,
  type AlertRule,
  type Channel,
  type ChannelConfig,
  type ChannelStatus,
  type ChannelType,
  type DigestSchedule,
  type Job,
  type Notification,
  type NotificationStatus,
  type Project,
  type RuleTestResult,
  type Trigger,
  type TriggerKind,
} from "./api";
import { Link, issuePath, projectsPath } from "./routes";
import { Ago, Button, Field, Header, Input, Notice } from "./ui";

const CHANNEL_TYPES: ChannelType[] = [
  "telegram",
  "slack",
  "discord",
  "webhook",
  "email",
];

const TRIGGER_KINDS: TriggerKind[] = [
  "new_issue",
  "regression",
  "issue_spike",
  "error_rate",
];

const WEEKDAYS = [
  "Monday",
  "Tuesday",
  "Wednesday",
  "Thursday",
  "Friday",
  "Saturday",
  "Sunday",
];

/** What each trigger watches, in the words somebody choosing one would use. */
const TRIGGER_DESCRIPTION: Record<TriggerKind, string> = {
  new_issue: "The first time a fingerprint is ever seen.",
  regression: "A resolved issue starts happening again.",
  issue_spike: "One issue's rate jumps over its own recent past.",
  error_rate:
    "The whole project crosses a rate. Counted before the rate limiter, so a flood cannot silence the alert about itself.",
};

/**
 * The fields each channel type needs.
 *
 * `type: "password"` is the whole of "this is a credential": it masks what is
 * typed and it is the only field the API will not hand back, so a second flag
 * saying the same thing would be a second thing to keep in step.
 */
interface FieldSpec {
  key: keyof ChannelConfig;
  label: string;
  placeholder?: string;
  type?: "text" | "password" | "number";
  optional?: boolean;
}

const CHANNEL_FIELDS: Record<ChannelType, FieldSpec[]> = {
  telegram: [
    {
      key: "bot_token",
      label: "Bot token",
      placeholder: "8100000000:AA…",
      type: "password",
    },
    { key: "chat_id", label: "Chat ID", placeholder: "-1001234567890" },
  ],
  slack: [
    {
      key: "url",
      label: "Slack webhook URL",
      placeholder: "https://hooks.slack.com/services/…",
      // Not marked secret, because it is required and typed in like any other
      // field — but its path is the credential, which is why the listing only
      // ever shows the origin back.
    },
  ],
  discord: [
    {
      key: "url",
      label: "Discord webhook URL",
      placeholder: "https://discord.com/api/webhooks/…",
    },
  ],
  webhook: [
    {
      key: "url",
      label: "Endpoint URL",
      placeholder: "https://example.com/trapline",
    },
    {
      key: "secret",
      label: "Signing secret",
      placeholder: "at least 16 characters",
      type: "password",
    },
  ],
  email: [
    { key: "host", label: "SMTP host", placeholder: "smtp.example.com" },
    { key: "port", label: "SMTP port", placeholder: "587", type: "number" },
    { key: "from", label: "From address", placeholder: "trapline@example.com" },
    {
      key: "to",
      label: "Recipients",
      placeholder: "ops@example.com, oncall@example.com",
    },
    { key: "username", label: "SMTP username", optional: true },
    {
      key: "password",
      label: "SMTP password",
      type: "password",
      optional: true,
    },
  ],
};

/** buildConfig turns the form's strings into the shape the API takes. */
function buildConfig(
  type: ChannelType,
  values: Record<string, string>,
): ChannelConfig {
  const config: ChannelConfig = {};
  for (const field of CHANNEL_FIELDS[type]) {
    const raw = (values[field.key] ?? "").trim();
    if (raw === "") continue;
    if (field.key === "port") config.port = Number(raw);
    else if (field.key === "to") {
      config.to = raw
        .split(",")
        .map((one) => one.trim())
        .filter(Boolean);
    } else {
      // Every remaining field on every type is a string, and the cast is
      // narrower than it looks: the keys come from CHANNEL_FIELDS, not from
      // the form.
      (config as Record<string, unknown>)[field.key] = raw;
    }
  }
  if (type === "email" && values["starttls"] === "on") config.starttls = true;
  return config;
}

/**
 * describeChannel is what the listing shows about a saved channel.
 *
 * Whatever the type, it is where the messages go and nothing that could be
 * pasted into a chat by whoever reads this screen.
 */
function describeChannel(channel: Channel): string {
  switch (channel.type) {
    case "telegram":
      return `chat ${channel.config.chat_id ?? "?"}`;
    case "slack":
    case "discord":
      return channel.config.url ?? "";
    case "webhook":
      return channel.config.url ?? "";
    case "email":
      return `${channel.config.host ?? "?"}:${channel.config.port ?? "?"} → ${(
        channel.config.to ?? []
      ).join(", ")}`;
  }
}

/** The four statuses, in words and colour — never colour alone. */
const STATUS_STYLES: Record<NotificationStatus, string> = {
  pending: "border-line text-muted",
  sent: "border-emerald-500/50 text-emerald-700 dark:text-emerald-400",
  failed: "border-amber-500/50 text-amber-700 dark:text-amber-400",
  dead: "border-red-500/60 text-red-700 dark:text-red-400",
};

/**
 * What each status means for whoever is waiting on the message.
 *
 * Spelled out rather than left to the word, because "failed" and "dead" read
 * as synonyms and are opposites in the only way that matters here: one is
 * still coming and the other is not. A row reaches `dead` either by spending
 * its ten attempts or because its channel was deleted underneath it, and the
 * row's own last_error says which — so this sentence says the part that is
 * true of both.
 */
const STATUS_MEANING: Record<NotificationStatus, string> = {
  pending: "queued; the next attempt has not come round yet",
  sent: "delivered",
  failed: "the last attempt failed; it will be retried on its own",
  dead: "given up on — it will NOT be retried unless you ask",
};

function StatusTag({ status }: { status: NotificationStatus }) {
  return (
    <span
      className={`shrink-0 rounded border px-1.5 py-0.5 text-xs font-medium tracking-wide uppercase ${STATUS_STYLES[status]}`}
      title={STATUS_MEANING[status]}
    >
      {status}
    </span>
  );
}

/** A default trigger of each kind: valid the moment it is chosen. */
function defaultTrigger(kind: TriggerKind): Trigger {
  switch (kind) {
    case "issue_spike":
      return { kind, window_s: 3600, min_count: 10, factor: 3 };
    case "error_rate":
      return { kind, window_s: 300, min_events_per_min: 60 };
    default:
      // new_issue and regression take no parameters, and the API refuses one
      // that carries any: a rule with a factor it will never read was written
      // by somebody expecting something it does not do.
      return { kind };
  }
}

/** describeTrigger renders a stored trigger as the sentence it means. */
function describeTrigger(trigger: Trigger): string {
  switch (trigger.kind) {
    case "new_issue":
      return "a new issue appears";
    case "regression":
      return "a resolved issue comes back";
    case "issue_spike":
      return `one issue does ${trigger.factor}× its baseline over ${(trigger.window_s ?? 0) / 60} min (at least ${trigger.min_count} events)`;
    case "error_rate":
      return `the project exceeds ${trigger.min_events_per_min} events/min over ${(trigger.window_s ?? 0) / 60} min`;
  }
}

function minutes(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)} min`;
  return `${Math.round(seconds / 360) / 10} h`;
}

export function Alerts({ onSignedOut }: { onSignedOut: () => void }) {
  const [channels, setChannels] = useState<Channel[]>([]);
  const [rules, setRules] = useState<AlertRule[]>([]);
  const [notifications, setNotifications] = useState<Notification[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [schedule, setSchedule] = useState<DigestSchedule | null>(null);
  const [health, setHealth] = useState<ChannelStatus[] | null>(null);
  const [statusFilter, setStatusFilter] = useState<NotificationStatus | "">("");

  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);

  const failed = useCallback(
    (err: unknown, fallback: string) => {
      if (err instanceof ApiError && err.isUnauthorized) {
        onSignedOut();
        return;
      }
      setError(err instanceof Error ? err.message : fallback);
    },
    [onSignedOut],
  );

  const refresh = useCallback(
    async (status: NotificationStatus | "") => {
      try {
        const [channelPage, rulePage, logPage, projectList, jobPage, digest] =
          await Promise.all([
            api.listChannels(),
            api.listRules(),
            api.listNotifications(status === "" ? undefined : status),
            api.listProjects(),
            api.listJobs(),
            api.getDigestSchedule(),
          ]);
        setChannels(channelPage.channels);
        setRules(rulePage.rules);
        setNotifications(logPage.notifications);
        setProjects(projectList);
        setJobs(jobPage.jobs);
        setSchedule(digest);
        setError("");
      } catch (err) {
        failed(err, "could not load the alerting configuration");
      } finally {
        setLoading(false);
      }
    },
    [failed],
  );

  useEffect(() => {
    void refresh(statusFilter);
  }, [refresh, statusFilter]);

  /** act runs one mutation, reports it, and reloads what it changed. */
  async function act(what: () => Promise<string>) {
    if (busy) return;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      setNotice(await what());
      await refresh(statusFilter);
    } catch (err) {
      failed(err, "something went wrong");
    } finally {
      setBusy(false);
    }
  }

  if (loading) return <p className="p-6 text-sm text-muted">loading…</p>;

  const digestChannels = channels.filter((channel) => channel.digest);
  const digestRunning = jobs.some((job) => job.name === "digest");
  const notifierRunning = jobs.some((job) => job.name === "notifier");

  return (
    <div className="mx-auto max-w-3xl space-y-10 p-6">
      <Header onSignedOut={onSignedOut}>
        <Link
          to={projectsPath}
          className="font-normal text-muted hover:text-accent"
        >
          Projects
        </Link>
        <span className="px-2 text-muted">/</span>
        Alerts
      </Header>

      <Notice>{error}</Notice>
      {notice ? (
        <p
          role="status"
          className="rounded-md border border-line px-3 py-2 text-sm text-muted"
        >
          {notice}
        </p>
      ) : null}

      <ChannelsSection
        channels={channels}
        health={health}
        notifierRunning={notifierRunning}
        busy={busy}
        onAdd={(type, name, config, digest) =>
          act(async () => {
            const created = await api.createChannel(type, name, config, digest);
            return `Channel ${created.name} saved. Its secrets are stored encrypted and will not be shown again.`;
          })
        }
        onRemove={(channel) =>
          act(async () => {
            await api.deleteChannel(channel.id);
            return `Channel ${channel.name} removed, and dropped from every rule that named it.`;
          })
        }
        onTest={(channel) =>
          act(async () => {
            await api.testChannel(channel.id);
            return `A test message was delivered to ${channel.name}.`;
          })
        }
        onCheck={() =>
          act(async () => {
            const report = await api.channelHealth();
            setHealth(report.channels);
            const broken = report.channels.filter((one) => !one.ok).length;
            return broken === 0
              ? "Every channel resolves and accepts connections. Nothing was sent, so this does not prove the credentials."
              : `${broken} of ${report.channels.length} channels could not be reached or decrypted.`;
          })
        }
      />

      <DigestSection
        schedule={schedule}
        digestChannels={digestChannels}
        running={digestRunning}
        busy={busy}
        onSave={(weekday, hour) =>
          act(async () => {
            const saved = await api.setDigestSchedule(weekday, hour);
            return `The weekly report will go out on ${saved.weekday} at ${String(saved.hour).padStart(2, "0")}:00 ${saved.timezone}.`;
          })
        }
      />

      <RulesSection
        rules={rules}
        channels={channels}
        projects={projects}
        busy={busy}
        onAdd={(rule) =>
          act(async () => {
            const created = await api.createRule(rule);
            return `Rule "${created.name}" is on.`;
          })
        }
        onRemove={(rule) =>
          act(async () => {
            await api.deleteRule(rule.id);
            return `Rule "${rule.name}" removed.`;
          })
        }
        onTest={(rule) =>
          act(async () => {
            const outcome = await api.testRule(rule.id);
            return describeRuleTest(outcome.results);
          })
        }
      />

      <LogSection
        notifications={notifications}
        channels={channels}
        status={statusFilter}
        busy={busy}
        onFilter={setStatusFilter}
        onRetry={(notification) =>
          act(async () => {
            await api.retryNotification(notification.id);
            return `Notification ${notification.id} is back at the front of the queue with its attempts restored.`;
          })
        }
      />
    </div>
  );
}

function describeRuleTest(results: RuleTestResult[]): string {
  const bad = results.filter((one) => !one.ok);
  if (bad.length === 0) {
    return `A sample alert was delivered to all ${results.length} channels.`;
  }
  return bad.map((one) => `${one.name}: ${one.error ?? "failed"}`).join(" · ");
}

function Section({
  title,
  children,
  description,
}: {
  title: string;
  description?: string;
  children: React.ReactNode;
}) {
  return (
    <section className="space-y-3">
      <h2 className="font-medium">{title}</h2>
      {description ? (
        <p className="text-sm text-muted">{description}</p>
      ) : null}
      {children}
    </section>
  );
}

function ChannelsSection({
  channels,
  health,
  notifierRunning,
  busy,
  onAdd,
  onRemove,
  onTest,
  onCheck,
}: {
  channels: Channel[];
  health: ChannelStatus[] | null;
  notifierRunning: boolean;
  busy: boolean;
  onAdd: (
    type: ChannelType,
    name: string,
    config: ChannelConfig,
    digest: boolean,
  ) => void;
  onRemove: (channel: Channel) => void;
  onTest: (channel: Channel) => void;
  onCheck: () => void;
}) {
  const [type, setType] = useState<ChannelType>("webhook");
  const [name, setName] = useState("");
  const [digest, setDigest] = useState(false);
  const [values, setValues] = useState<Record<string, string>>({});

  const fields = CHANNEL_FIELDS[type];
  const complete =
    name.trim() !== "" &&
    fields.every(
      (field) => field.optional || (values[field.key] ?? "").trim() !== "",
    );

  const byID = new Map((health ?? []).map((one) => [one.id, one]));

  return (
    <Section
      title="Channels"
      description="Where a notification goes. Each type needs different things, so the form is the one for the type you picked rather than twenty fields of which nineteen are wrong."
    >
      {channels.length === 0 ? (
        <p className="text-sm text-muted">
          No channels yet. Until there is one, nothing is notified when
          something breaks — and the notifier has no goroutine at all, which is
          what makes a subsystem you do not use cost nothing.
        </p>
      ) : (
        <ul className="space-y-3">
          {channels.map((channel) => {
            const status = byID.get(channel.id);
            return (
              <li
                key={channel.id}
                className="flex flex-wrap items-baseline gap-x-3 gap-y-1 border-b border-line pb-3"
              >
                <span className="rounded border border-line px-1.5 py-0.5 text-xs tracking-wide uppercase">
                  {channel.type}
                </span>
                <span className="font-medium">{channel.name}</span>
                <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted">
                  {describeChannel(channel)}
                </span>
                {channel.digest ? (
                  <span className="shrink-0 rounded border border-line px-1.5 py-0.5 text-xs text-muted">
                    weekly digest
                  </span>
                ) : null}
                <Button
                  variant="quiet"
                  disabled={busy}
                  onClick={() => onTest(channel)}
                >
                  Test
                </Button>
                <Button
                  variant="danger"
                  disabled={busy}
                  onClick={() => onRemove(channel)}
                >
                  Remove
                </Button>
                {status ? (
                  <p
                    className={`w-full text-xs ${status.ok ? "text-muted" : "text-red-600"}`}
                  >
                    {status.ok ? "reachable" : "unreachable"} —{" "}
                    {status.detail}
                  </p>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}

      {channels.length > 0 ? (
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="quiet" disabled={busy} onClick={onCheck}>
            Check channels
          </Button>
          <span className="text-xs text-muted">
            Resolves each host, opens a connection and hangs up. It sends
            nothing, so it cannot tell you a token is still valid — only
            &ldquo;Test&rdquo; can, and that posts a real message.
          </span>
        </div>
      ) : null}

      <p className="text-xs text-muted">
        {notifierRunning
          ? "The notifier is running: queued notifications are being delivered."
          : "The notifier is not running, because no channel is configured."}
      </p>

      <form
        className="space-y-3 rounded-md border border-line p-4"
        onSubmit={(event) => {
          event.preventDefault();
          if (!complete) return;
          onAdd(type, name.trim(), buildConfig(type, values), digest);
          setName("");
          setValues({});
          setDigest(false);
        }}
      >
        <h3 className="text-sm font-medium">Add a channel</h3>

        <label className="block">
          <span className="mb-1 block text-sm font-medium">Channel type</span>
          <select
            className="w-full rounded-md border border-line bg-transparent px-3 py-2 text-sm outline-none focus:border-accent"
            value={type}
            onChange={(event) => {
              setType(event.target.value as ChannelType);
              // The fields of the previous type are not the fields of this
              // one, and carrying a value across would submit a Slack channel
              // holding a bot token — which the API rejects, correctly, but
              // only after the operator wondered why.
              setValues({});
            }}
          >
            {CHANNEL_TYPES.map((one) => (
              <option key={one} value={one}>
                {one}
              </option>
            ))}
          </select>
        </label>

        <Field
          label="Channel name"
          value={name}
          onChange={(event) => setName(event.target.value)}
          placeholder="#incidents"
        />

        {fields.map((field) => (
          <Field
            key={String(field.key)}
            label={field.label}
            type={field.type ?? "text"}
            placeholder={field.placeholder ?? ""}
            value={values[field.key] ?? ""}
            onChange={(event) =>
              setValues((previous) => ({
                ...previous,
                [field.key]: event.target.value,
              }))
            }
          />
        ))}

        {type === "email" ? (
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              className="accent-accent"
              checked={values["starttls"] === "on"}
              onChange={(event) =>
                setValues((previous) => ({
                  ...previous,
                  starttls: event.target.checked ? "on" : "",
                }))
              }
            />
            Upgrade the session with STARTTLS
          </label>
        ) : null}

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            className="accent-accent"
            checked={digest}
            onChange={(event) => setDigest(event.target.checked)}
          />
          Also send the weekly digest here
        </label>

        <p className="text-xs text-muted">
          Secrets are encrypted before they are stored and are never shown
          again — not here, not in the API, not in the CLI. To change one,
          remove the channel and add it back.
        </p>

        <Button type="submit" disabled={!complete || busy}>
          Save channel
        </Button>
      </form>
    </Section>
  );
}

function DigestSection({
  schedule,
  digestChannels,
  running,
  busy,
  onSave,
}: {
  schedule: DigestSchedule | null;
  digestChannels: Channel[];
  running: boolean;
  busy: boolean;
  onSave: (weekday: string, hour: number) => void;
}) {
  const [weekday, setWeekday] = useState(schedule?.weekday ?? "Monday");
  const [hour, setHour] = useState(schedule?.hour ?? 9);

  useEffect(() => {
    if (!schedule) return;
    setWeekday(schedule.weekday);
    setHour(schedule.hour);
  }, [schedule]);

  return (
    <Section
      title="Weekly digest"
      description="One report per week: what appeared, what came back, what was loudest, and how the week compares to the one before it."
    >
      {/* The gap ADR 014 left open, closed where somebody would look for it: a
          job that is not running says why it is not, instead of leaving an
          operator to wonder which of several things they forgot. */}
      <p className="text-sm">
        {running ? (
          <>
            Scheduled, and going out to{" "}
            {digestChannels.map((one) => one.name).join(", ")}.
          </>
        ) : (
          <>
            Not scheduled: no channel has asked for it. Tick &ldquo;Also send
            the weekly digest here&rdquo; on a channel and the job starts
            immediately — no restart. Until then{" "}
            <code className="font-mono text-xs">trapline digest preview</code>{" "}
            still shows what it would say.
          </>
        )}
      </p>

      <form
        className="flex flex-wrap items-end gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          onSave(weekday, hour);
        }}
      >
        <label className="block">
          <span className="mb-1 block text-sm font-medium">Day</span>
          <select
            className="rounded-md border border-line bg-transparent px-3 py-2 text-sm outline-none focus:border-accent"
            value={weekday}
            onChange={(event) => setWeekday(event.target.value)}
          >
            {WEEKDAYS.map((day) => (
              <option key={day} value={day}>
                {day}
              </option>
            ))}
          </select>
        </label>

        <div className="w-24">
          <label className="block">
            <span className="mb-1 block text-sm font-medium">Hour</span>
            <Input
              type="number"
              min={0}
              max={23}
              inputMode="numeric"
              aria-label="Hour"
              value={String(hour)}
              onChange={(event) => setHour(Number(event.target.value))}
            />
          </label>
        </div>

        <span className="pb-2 text-sm text-muted">
          {schedule?.timezone ?? "UTC"}
        </span>

        <Button type="submit" disabled={busy}>
          Save schedule
        </Button>
      </form>
    </Section>
  );
}

function RulesSection({
  rules,
  channels,
  projects,
  busy,
  onAdd,
  onRemove,
  onTest,
}: {
  rules: AlertRule[];
  channels: Channel[];
  projects: Project[];
  busy: boolean;
  onAdd: (rule: {
    project_id: number | null;
    name: string;
    trigger: Trigger;
    channel_ids: number[];
    silence_seconds: number;
  }) => void;
  onRemove: (rule: AlertRule) => void;
  onTest: (rule: AlertRule) => void;
}) {
  const [name, setName] = useState("");
  const [scope, setScope] = useState("");
  const [trigger, setTrigger] = useState<Trigger>(defaultTrigger("new_issue"));
  const [selected, setSelected] = useState<number[]>([]);
  const [silence, setSilence] = useState(900);

  const projectName = (id: number | null) =>
    id === null
      ? "all projects"
      : (projects.find((one) => one.id === id)?.name ?? `#${id}`);

  const complete = name.trim() !== "" && selected.length > 0;

  function setParameter(key: keyof Trigger, value: number) {
    setTrigger((previous) => ({ ...previous, [key]: value }));
  }

  return (
    <Section
      title="Rules"
      description="What is worth being told about, and where. There is no query language: four named conditions with numbers, which is what an error tracker is actually asked."
    >
      {rules.length === 0 ? (
        <p className="text-sm text-muted">
          No rules yet. Channels without a rule deliver nothing.
        </p>
      ) : (
        <ul className="space-y-3">
          {rules.map((rule) => (
            <li
              key={rule.id}
              className="flex flex-wrap items-baseline gap-x-3 gap-y-1 border-b border-line pb-3"
            >
              <span className="font-medium">{rule.name}</span>
              <span className="min-w-0 flex-1 text-sm text-muted">
                on {projectName(rule.project_id)} when{" "}
                {describeTrigger(rule.trigger)}, then quiet for{" "}
                {minutes(rule.silence_seconds)}
              </span>
              <span className="shrink-0 text-xs text-muted">
                →{" "}
                {rule.channel_ids
                  .map(
                    (id) =>
                      channels.find((one) => one.id === id)?.name ?? `#${id}`,
                  )
                  .join(", ")}
              </span>
              <Button variant="quiet" disabled={busy} onClick={() => onTest(rule)}>
                Test
              </Button>
              <Button
                variant="danger"
                disabled={busy}
                onClick={() => onRemove(rule)}
              >
                Remove
              </Button>
            </li>
          ))}
        </ul>
      )}

      <form
        className="space-y-3 rounded-md border border-line p-4"
        onSubmit={(event) => {
          event.preventDefault();
          if (!complete) return;
          onAdd({
            project_id: scope === "" ? null : Number(scope),
            name: name.trim(),
            trigger,
            channel_ids: selected,
            silence_seconds: silence,
          });
          setName("");
          setSelected([]);
        }}
      >
        <h3 className="text-sm font-medium">Add a rule</h3>

        <Field
          label="Rule name"
          value={name}
          onChange={(event) => setName(event.target.value)}
          placeholder="anything new in production"
        />

        <label className="block">
          <span className="mb-1 block text-sm font-medium">Project</span>
          <select
            className="w-full rounded-md border border-line bg-transparent px-3 py-2 text-sm outline-none focus:border-accent"
            value={scope}
            onChange={(event) => setScope(event.target.value)}
          >
            <option value="">All projects</option>
            {projects.map((project) => (
              <option key={project.id} value={String(project.id)}>
                {project.name}
              </option>
            ))}
          </select>
        </label>

        <label className="block">
          <span className="mb-1 block text-sm font-medium">Trigger</span>
          <select
            className="w-full rounded-md border border-line bg-transparent px-3 py-2 text-sm outline-none focus:border-accent"
            value={trigger.kind}
            onChange={(event) =>
              // A whole new trigger rather than a changed kind: the parameters
              // of the previous one are not this one's, and the API refuses a
              // trigger carrying a field its kind never reads.
              setTrigger(defaultTrigger(event.target.value as TriggerKind))
            }
          >
            {TRIGGER_KINDS.map((kind) => (
              <option key={kind} value={kind}>
                {kind}
              </option>
            ))}
          </select>
        </label>
        <p className="text-xs text-muted">
          {TRIGGER_DESCRIPTION[trigger.kind]}
        </p>

        {trigger.kind === "issue_spike" ? (
          <div className="flex flex-wrap gap-3">
            <div className="w-32">
              <Field
                label="Window (s)"
                type="number"
                min={60}
                value={String(trigger.window_s ?? 0)}
                onChange={(event) =>
                  setParameter("window_s", Number(event.target.value))
                }
              />
            </div>
            <div className="w-32">
              <Field
                label="Minimum events"
                type="number"
                min={1}
                value={String(trigger.min_count ?? 0)}
                onChange={(event) =>
                  setParameter("min_count", Number(event.target.value))
                }
              />
            </div>
            <div className="w-32">
              <Field
                label="Factor"
                type="number"
                min={1}
                step="0.5"
                value={String(trigger.factor ?? 0)}
                onChange={(event) =>
                  setParameter("factor", Number(event.target.value))
                }
              />
            </div>
          </div>
        ) : null}

        {trigger.kind === "error_rate" ? (
          <div className="flex flex-wrap gap-3">
            <div className="w-32">
              <Field
                label="Window (s)"
                type="number"
                min={60}
                max={300}
                value={String(trigger.window_s ?? 0)}
                onChange={(event) =>
                  setParameter("window_s", Number(event.target.value))
                }
              />
            </div>
            <div className="w-40">
              <Field
                label="Events per minute"
                type="number"
                min={1}
                value={String(trigger.min_events_per_min ?? 0)}
                onChange={(event) =>
                  setParameter(
                    "min_events_per_min",
                    Number(event.target.value),
                  )
                }
              />
            </div>
          </div>
        ) : null}

        <fieldset className="space-y-1">
          <legend className="mb-1 text-sm font-medium">Channels</legend>
          {channels.length === 0 ? (
            <p className="text-xs text-muted">
              Add a channel first: a rule with nowhere to deliver is a rule
              that looks configured and notifies nobody.
            </p>
          ) : (
            channels.map((channel) => (
              <label
                key={channel.id}
                className="flex items-center gap-2 text-sm"
              >
                <input
                  type="checkbox"
                  className="accent-accent"
                  checked={selected.includes(channel.id)}
                  onChange={(event) =>
                    setSelected((previous) =>
                      event.target.checked
                        ? [...previous, channel.id]
                        : previous.filter((id) => id !== channel.id),
                    )
                  }
                />
                {channel.name}{" "}
                <span className="text-xs text-muted">({channel.type})</span>
              </label>
            ))
          )}
        </fieldset>

        <div className="w-40">
          <Field
            label="Silence (seconds)"
            type="number"
            min={0}
            value={String(silence)}
            onChange={(event) => setSilence(Number(event.target.value))}
          />
        </div>
        <p className="text-xs text-muted">
          How long the rule stays quiet about the same subject after it fires.
          A deploy that breaks one endpoint should send one message, not four
          hundred.
        </p>

        <Button type="submit" disabled={!complete || busy}>
          Save rule
        </Button>
      </form>
    </Section>
  );
}

function LogSection({
  notifications,
  channels,
  status,
  busy,
  onFilter,
  onRetry,
}: {
  notifications: Notification[];
  channels: Channel[];
  status: NotificationStatus | "";
  busy: boolean;
  onFilter: (status: NotificationStatus | "") => void;
  onRetry: (notification: Notification) => void;
}) {
  const filters: (NotificationStatus | "")[] = [
    "",
    "pending",
    "sent",
    "failed",
    "dead",
  ];

  return (
    <Section
      title="Delivery log"
      description="Every notification this installation queued, and what became of it. It answers the question that has no other answer: why did I not get one."
    >
      <div className="flex flex-wrap gap-2">
        {filters.map((one) => (
          <button
            key={one || "all"}
            type="button"
            aria-pressed={status === one}
            className={`rounded-md border px-2 py-1 text-xs transition ${
              status === one
                ? "border-accent text-accent"
                : "border-line text-muted hover:text-ink"
            }`}
            onClick={() => onFilter(one)}
          >
            {one === "" ? "All" : one}
          </button>
        ))}
      </div>

      {notifications.length === 0 ? (
        <p className="text-sm text-muted">
          {status === ""
            ? "Nothing has been queued yet."
            : `No ${status} notifications.`}
        </p>
      ) : (
        <ul className="space-y-3">
          {notifications.map((notification) => {
            const channel = channels.find(
              (one) => one.id === notification.channel_id,
            );
            const retryable =
              notification.status === "dead" ||
              notification.status === "failed";
            return (
              <li
                key={notification.id}
                className="space-y-1 border-b border-line pb-3"
              >
                <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                  <StatusTag status={notification.status} />
                  <span className="font-mono text-xs text-muted">
                    {notification.payload.event}
                  </span>
                  <span className="min-w-0 flex-1 truncate text-sm">
                    {notification.payload.title ||
                      notification.payload.rule ||
                      notification.subject_key}
                  </span>
                  <span className="shrink-0 text-xs text-muted">
                    {channel?.name ?? `channel #${notification.channel_id}`} ·{" "}
                    <Ago at={notification.created_at} />
                  </span>
                  {retryable ? (
                    <Button
                      variant="quiet"
                      disabled={busy}
                      onClick={() => onRetry(notification)}
                    >
                      Retry
                    </Button>
                  ) : null}
                </div>

                {/* The sentence, not just the word. "failed" and "dead" look
                    like synonyms and are opposites: one is still coming. */}
                <p className="text-xs text-muted">
                  {STATUS_MEANING[notification.status]}
                  {notification.attempts > 0
                    ? ` · ${notification.attempts} attempt${notification.attempts === 1 ? "" : "s"}`
                    : ""}
                  {notification.status === "failed" ? (
                    <>
                      {" "}
                      · next attempt{" "}
                      <Ago at={notification.next_attempt_at} />
                    </>
                  ) : null}
                </p>

                {notification.last_error ? (
                  <p className="font-mono text-xs break-all text-red-600">
                    {notification.last_error}
                  </p>
                ) : null}

                {notification.payload.issue_id &&
                notification.payload.project_id ? (
                  <Link
                    to={issuePath(
                      notification.payload.project_id,
                      notification.payload.issue_id,
                    )}
                    className="text-xs text-accent hover:underline"
                  >
                    Open the issue this was about
                  </Link>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
    </Section>
  );
}
