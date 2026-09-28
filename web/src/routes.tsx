// Client-side routes.
//
// Hand-written rather than a router dependency: there are three screens, and
// the only reason the panel needs URLs at all is that an issue has to be
// linkable — the address you paste into a chat when you want someone to look
// at what broke. The server already serves index.html for unknown paths, so a
// pasted link opens the issue instead of a 404.

import type { ReactNode } from "react";
import { useEffect, useState } from "react";

export type Route =
  | { kind: "projects" }
  | { kind: "dashboard"; projectID: number }
  | { kind: "issues"; projectID: number }
  | { kind: "issue"; projectID: number; issueID: number }
  | { kind: "performance"; projectID: number; transaction?: string }
  | { kind: "trace"; projectID: number; traceID: string }
  | { kind: "releases"; projectID: number }
  | { kind: "release"; projectID: number; version: string }
  | { kind: "settings"; projectID: number }
  | { kind: "monitors"; projectID: number }
  | { kind: "alerts" };

export const projectsPath = "/";

/**
 * alertsPath is one screen for the whole installation, not one per project.
 *
 * Channels belong to the installation — a Slack workspace is not a property
 * of one application — and a rule may cover one project or all of them. Two
 * copies of the same channel under two projects is the shape that produces
 * four identical messages during one incident.
 */
export const alertsPath = "/alerts";

export function dashboardPath(projectID: number): string {
  return `/projects/${projectID}`;
}

/**
 * issuesPath, optionally pre-filtered.
 *
 * The filters ride in the query string rather than the path because they are
 * how a link is shared: "look, it is only Safari" is a URL somebody pastes
 * into a chat, and it has to reopen the list already narrowed.
 */
export function issuesPath(
  projectID: number,
  filters?: { tag?: string; release?: string; environment?: string },
): string {
  const base = `/projects/${projectID}/issues`;
  if (!filters) return base;
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (value) params.set(key, value);
  }
  const query = params.toString();
  return query ? `${base}?${query}` : base;
}

/**
 * performancePath, optionally pointing at one transaction.
 *
 * The transaction name is a path segment rather than a query parameter for
 * the reason the API puts it there too: it is an identifier, not a filter,
 * and "this endpoint got slower after the deploy" is a link somebody pastes
 * into a chat.
 */
export function performancePath(projectID: number, transaction?: string): string {
  const base = `/projects/${projectID}/performance`;
  if (!transaction) return base;
  return `${base}/${encodeURIComponent(transaction)}`;
}

export function tracePath(projectID: number, traceID: string): string {
  return `/projects/${projectID}/traces/${encodeURIComponent(traceID)}`;
}

export function releasesPath(projectID: number): string {
  return `/projects/${projectID}/releases`;
}

export function releasePath(projectID: number, version: string): string {
  return `/projects/${projectID}/releases/${encodeURIComponent(version)}`;
}

export function issuePath(projectID: number, issueID: number): string {
  return `/projects/${projectID}/issues/${issueID}`;
}

export function settingsPath(projectID: number): string {
  return `/projects/${projectID}/settings`;
}

/**
 * monitorsPath is one screen for both families.
 *
 * Cron and uptime are one concept watched from two sides (ADR 037), so they
 * share a URL as well as a screen: the notification links this product sends
 * carry the family in their path (/monitors/cron/7), and they land here.
 */
export function monitorsPath(projectID: number): string {
  return `/projects/${projectID}/monitors`;
}

const ISSUES = /^\/projects\/(\d+)\/issues\/?$/;
const ISSUE = /^\/projects\/(\d+)\/issues\/(\d+)\/?$/;
const SETTINGS = /^\/projects\/(\d+)\/settings\/?$/;
// The trailing segments are what a notification's link carries — a family and
// an id — and they land on the same screen: there is one page for monitors,
// and deep-linking to one of them is a scroll, not a route.
const MONITORS = /^\/projects\/(\d+)\/monitors(\/(cron|uptime)(\/\d+)?)?\/?$/;
// A transaction name is whatever an SDK put on a span — `GET /orders/{id}`,
// slashes and all — so it is percent-encoded into one segment and read back
// through decodeURIComponent, exactly like a release version.
const PERFORMANCE = /^\/projects\/(\d+)\/performance(?:\/([^/]+))?\/?$/;
const TRACE = /^\/projects\/(\d+)\/traces\/([^/]+)\/?$/;
const RELEASES = /^\/projects\/(\d+)\/releases\/?$/;
// A release version is whatever a deploy tool put on an event, so the segment
// is anything but a slash and is read back through decodeURIComponent.
const RELEASE = /^\/projects\/(\d+)\/releases\/([^/]+)\/?$/;
const DASHBOARD = /^\/projects\/(\d+)\/?$/;
const ALERTS = /^\/alerts\/?$/;

export function parseRoute(pathname: string): Route {
  if (ALERTS.test(pathname)) return { kind: "alerts" };

  const issue = ISSUE.exec(pathname);
  if (issue?.[1] && issue[2]) {
    return {
      kind: "issue",
      projectID: Number(issue[1]),
      issueID: Number(issue[2]),
    };
  }
  const issues = ISSUES.exec(pathname);
  if (issues?.[1]) {
    return { kind: "issues", projectID: Number(issues[1]) };
  }
  const settings = SETTINGS.exec(pathname);
  if (settings?.[1]) {
    return { kind: "settings", projectID: Number(settings[1]) };
  }
  const monitors = MONITORS.exec(pathname);
  if (monitors?.[1]) {
    return { kind: "monitors", projectID: Number(monitors[1]) };
  }
  const trace = TRACE.exec(pathname);
  if (trace?.[1] && trace[2]) {
    return {
      kind: "trace",
      projectID: Number(trace[1]),
      traceID: decodeURIComponent(trace[2]),
    };
  }
  const performance = PERFORMANCE.exec(pathname);
  if (performance?.[1]) {
    const transaction = performance[2];
    return {
      kind: "performance",
      projectID: Number(performance[1]),
      ...(transaction ? { transaction: decodeURIComponent(transaction) } : {}),
    };
  }
  const release = RELEASE.exec(pathname);
  if (release?.[1] && release[2]) {
    return {
      kind: "release",
      projectID: Number(release[1]),
      version: decodeURIComponent(release[2]),
    };
  }
  const releases = RELEASES.exec(pathname);
  if (releases?.[1]) {
    return { kind: "releases", projectID: Number(releases[1]) };
  }
  const dashboard = DASHBOARD.exec(pathname);
  if (dashboard?.[1]) {
    return { kind: "dashboard", projectID: Number(dashboard[1]) };
  }
  // Anything else is the projects list rather than a not-found screen: an
  // unknown path here is a typo or a stale link, and the useful answer to
  // both is the screen every other screen is reached from.
  return { kind: "projects" };
}

export function navigate(path: string): void {
  window.history.pushState(null, "", path);
  window.scrollTo(0, 0);
  window.dispatchEvent(new PopStateEvent("popstate"));
}

export function useRoute(): Route {
  const [route, setRoute] = useState<Route>(() =>
    parseRoute(window.location.pathname),
  );

  useEffect(() => {
    const read = () => setRoute(parseRoute(window.location.pathname));
    window.addEventListener("popstate", read);
    return () => window.removeEventListener("popstate", read);
  }, []);

  return route;
}

/** The tag-shaped filters an issue listing can be narrowed by. */
export interface IssueFilters {
  environment: string;
  release: string;
  tag: string;
}

/** readIssueFilters reads them out of the address bar. */
export function readIssueFilters(): IssueFilters {
  const params = new URLSearchParams(window.location.search);
  return {
    environment: params.get("environment") ?? "",
    release: params.get("release") ?? "",
    tag: params.get("tag") ?? "",
  };
}

/**
 * useIssueFilters keeps the filters in the address bar without filling the
 * back button with keystrokes.
 *
 * The filters belong in the URL because they are what somebody shares — "look,
 * it is only Safari" is a link, and the link has to reopen the list already
 * narrowed. But a history entry per character typed into the environment box
 * would mean pressing back thirteen times to leave a page. So the URL is
 * *replaced* rather than pushed, and a real navigation — a link from the
 * dashboard, the back button, a pasted address — resynchronises it here.
 */
export function useIssueFilters(): [IssueFilters, (next: IssueFilters) => void] {
  const [filters, setFilters] = useState<IssueFilters>(readIssueFilters);

  useEffect(() => {
    const resync = () => setFilters(readIssueFilters());
    window.addEventListener("popstate", resync);
    return () => window.removeEventListener("popstate", resync);
  }, []);

  const apply = (next: IssueFilters) => {
    setFilters(next);
    const params = new URLSearchParams(window.location.search);
    for (const [name, value] of Object.entries(next)) {
      if (value) params.set(name, value);
      else params.delete(name);
    }
    const query = params.toString();
    window.history.replaceState(
      null,
      "",
      window.location.pathname + (query ? `?${query}` : ""),
    );
  };

  return [filters, apply];
}

export function Link({
  to,
  className,
  children,
  current,
  label,
}: {
  to: string;
  className?: string;
  children: ReactNode;
  /**
   * The accessible name, when the visible text is not one.
   *
   * A tag value like `web-01` says what the link points at and nothing about
   * what following it does, which is the whole of WCAG's "link purpose". An
   * explicit prop rather than letting callers pass `aria-label` through:
   * TypeScript does not check hyphenated JSX attributes on a component, so one
   * spelled that way is dropped in silence — which is exactly how the tag
   * links shipped with the wrong name and only a browser noticed.
   */
  label?: string;
  /**
   * Marks the link that points at the page already open.
   *
   * A prop rather than letting callers spread arbitrary anchor attributes:
   * this is the one attribute a navigation needs from the outside, and the
   * alternative is a component whose contract is "anything an <a> takes",
   * which is how the click handling below quietly gets overridden.
   */
  current?: boolean;
}) {
  return (
    <a
      href={to}
      className={className}
      aria-current={current ? "page" : undefined}
      aria-label={label}
      onClick={(event) => {
        // A real href, and the gestures that mean "open this somewhere else"
        // are left to the browser. Swallowing them would break the one thing
        // these links exist for: copying and sharing the address.
        if (
          event.defaultPrevented ||
          event.button !== 0 ||
          event.metaKey ||
          event.ctrlKey ||
          event.shiftKey ||
          event.altKey
        ) {
          return;
        }
        event.preventDefault();
        navigate(to);
      }}
    >
      {children}
    </a>
  );
}
