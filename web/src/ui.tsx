import type { ReactNode } from "react";
import { useState } from "react";
import { api, type IssueStatus, type Level } from "./api";
import {
  Link,
  alertsPath,
  dashboardPath,
  issuesPath,
  monitorsPath,
  performancePath,
  releasesPath,
  settingsPath,
} from "./routes";

export function Button({
  children,
  onClick,
  href,
  type = "button",
  variant = "primary",
  disabled,
}: {
  children: ReactNode;
  onClick?: () => void;
  href?: string;
  type?: "button" | "submit";
  variant?: "primary" | "quiet" | "danger";
  disabled?: boolean;
}) {
  const styles = {
    primary: "bg-accent text-white hover:opacity-90",
    quiet: "border border-line hover:bg-line/40",
    danger: "border border-line text-red-600 hover:bg-red-500/10",
  }[variant];
  const className = `rounded-md px-3 py-1.5 text-sm font-medium transition disabled:opacity-50 ${styles}`;

  // A button that navigates is a link wearing a button's clothes. Rendering a
  // real anchor keeps "open in a new tab" and "copy link address" working,
  // which is the whole point of the panel having addressable screens.
  if (href !== undefined) {
    return (
      <Link to={href} className={`inline-block ${className}`}>
        {children}
      </Link>
    );
  }

  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className={className}
    >
      {children}
    </button>
  );
}

const inputStyles =
  "w-full rounded-md border border-line bg-transparent px-3 py-2 text-sm outline-none focus:border-accent";

export function Input(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={inputStyles} />;
}

export function Field({
  label,
  ...props
}: { label: string } & React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <label className="block">
      <span className="mb-1 block text-sm font-medium">{label}</span>
      <Input {...props} />
    </label>
  );
}

// Severity carries colour, but never only colour: the word is always there,
// because a red dot means nothing to a reader who cannot see it is red, and
// nothing to anyone at all in a screenshot pasted into a ticket.
const levelStyles: Record<Level, string> = {
  fatal: "border-red-500/50 text-red-600 dark:text-red-400",
  error: "border-red-500/40 text-red-600 dark:text-red-400",
  warning: "border-amber-500/50 text-amber-600 dark:text-amber-400",
  info: "border-line text-muted",
  debug: "border-line text-muted",
};

export function LevelTag({ level }: { level: Level }) {
  return (
    <span
      className={`shrink-0 rounded border px-1.5 py-0.5 text-xs tracking-wide uppercase ${levelStyles[level]}`}
    >
      {level}
    </span>
  );
}

/**
 * StatusTag renders anything but the default status.
 *
 * Unresolved is the state of almost every row on the screen someone opens
 * when something is broken; labelling all of them would be noise that hides
 * the two rows where the state is the news.
 */
export function StatusTag({ status }: { status: IssueStatus }) {
  if (status === "unresolved") return null;
  return (
    <span className="shrink-0 rounded border border-line px-1.5 py-0.5 text-xs text-muted">
      {status}
    </span>
  );
}

function timeAgo(date: Date, now: number): string {
  const seconds = Math.round((now - date.getTime()) / 1000);
  // A client clock running ahead of the server's is common enough that an
  // event "in 4 minutes" would be a recurring false alarm.
  if (seconds < 60) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  if (days < 30) return `${days}d ago`;
  return date.toLocaleDateString(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

export function Ago({ at }: { at: string }) {
  const date = new Date(at);
  if (Number.isNaN(date.getTime())) return <span>unknown</span>;
  return (
    <time dateTime={at} title={date.toLocaleString()}>
      {timeAgo(date, Date.now())}
    </time>
  );
}

/**
 * Header is the title bar every screen wears.
 *
 * Shared rather than repeated because the way out is part of it: an operator
 * three clicks deep in a stacktrace should not have to walk back to the first
 * screen to sign out of a machine they are leaving.
 */
export function Header({
  children,
  onSignedOut,
}: {
  children: ReactNode;
  onSignedOut: () => void;
}) {
  async function signOut() {
    try {
      await api.logout();
    } finally {
      // A logout that failed still means someone asked to leave. Re-checking
      // the session decides what they see next, and if the cookie somehow
      // survived they land back where they were rather than in limbo.
      onSignedOut();
    }
  }

  return (
    <header className="flex items-baseline justify-between gap-4 border-b border-line pb-4">
      <h1 className="min-w-0 truncate text-lg font-semibold">{children}</h1>
      <div className="flex shrink-0 gap-2">
        {/* Alerting belongs to the installation rather than to a project, so
            it hangs here rather than in ProjectNav — and it is on every
            screen because the moment somebody wants it is the moment they are
            three clicks deep in an incident. */}
        <Button variant="quiet" href={alertsPath}>
          Alerts
        </Button>
        <Button variant="quiet" onClick={() => void signOut()}>
          Sign out
        </Button>
      </div>
    </header>
  );
}

/**
 * ProjectNav is the six screens a project has, always in the same order.
 *
 * Shared rather than repeated per screen because the point of it is that it
 * does not move: somebody who learned where "Releases" is during one incident
 * should find it in the same place during the next, and four hand-rolled
 * header rows drift apart the first time one of them gains a screen.
 */
export function ProjectNav({
  projectID,
  current,
}: {
  projectID: number;
  current:
    | "dashboard"
    | "issues"
    | "performance"
    | "releases"
    | "monitors"
    | "settings";
}) {
  const tabs = [
    { key: "dashboard", label: "Dashboard", to: dashboardPath(projectID) },
    { key: "issues", label: "Issues", to: issuesPath(projectID) },
    // Next to Issues rather than at the end, because it is the same question
    // asked about latency instead of errors, and the two get opened one after
    // the other during an incident.
    { key: "performance", label: "Performance", to: performancePath(projectID) },
    { key: "releases", label: "Releases", to: releasesPath(projectID) },
    { key: "monitors", label: "Monitors", to: monitorsPath(projectID) },
    { key: "settings", label: "Settings", to: settingsPath(projectID) },
  ] as const;

  return (
    <nav aria-label="Project sections" className="flex gap-1 border-b border-line">
      {tabs.map((tab) => {
        const active = tab.key === current;
        return (
          <Link
            key={tab.key}
            to={tab.to}
            // aria-current is what tells a screen reader which of four links
            // is the page already open. The underline says it to everyone
            // else, and neither is doing the job alone.
            className={`-mb-px border-b-2 px-3 py-2 text-sm transition ${
              active
                ? "border-accent font-medium"
                : "border-transparent text-muted hover:text-ink"
            }`}
            current={active}
          >
            {tab.label}
          </Link>
        );
      })}
    </nav>
  );
}

/**
 * ReleaseTag renders a version the way it is read: monospaced, and never
 * silently absent.
 *
 * "No release" is a fact about a deploy pipeline that is not reporting one,
 * and it is the difference between "which build broke this" having an answer
 * and having none. Rendering nothing would hide the question.
 */
export function ReleaseTag({ version }: { version?: string | undefined }) {
  if (!version) return <span className="text-muted">no release</span>;
  return <span className="font-mono break-all">{version}</span>;
}

export function Notice({ children }: { children: ReactNode }) {
  if (!children) return null;
  return (
    <p className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-600">
      {children}
    </p>
  );
}

/**
 * Copy renders a value with a copy button.
 *
 * It exists because of what the value is: a DSN is meant to be pasted into an
 * SDK config, and selecting a 60-character string out of a table by hand is
 * where people truncate it and then spend an hour wondering why no events
 * arrive.
 */
export function Copy({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // The clipboard API needs a secure context, so it is unavailable over
      // plain HTTP — which is exactly how many self-hosted installs run. The
      // value stays selectable, so failing quietly here degrades to the
      // behaviour someone would have had anyway.
    }
  }

  return (
    <span className="inline-flex items-center gap-2">
      <code className="rounded bg-line/50 px-2 py-1 font-mono text-xs break-all">
        {value}
      </code>
      <button
        type="button"
        onClick={copy}
        className="shrink-0 text-xs text-muted hover:text-accent"
      >
        {copied ? "copied" : "copy"}
      </button>
    </span>
  );
}
