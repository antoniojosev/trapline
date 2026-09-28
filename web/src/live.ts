// The live feed the issue list keeps open while somebody is reading it.
//
// Server-sent events, over the browser's own EventSource: no library, no
// upgrade handshake, and it reconnects by itself when a laptop wakes up or a
// deploy restarts the server. The session cookie rides along because the
// request is same-origin, which is the whole reason a header-based bearer
// token is not needed here.

import { useEffect, useRef, useState } from "react";
import type { Level } from "./api";

/** What the server says happened. */
export type LiveEventKind = "issue.new" | "issue.regressed" | "issue.updated";

export interface LiveIssue {
  issue_id: number;
  project_id: number;
  title: string;
  culprit: string;
  level: Level;
  status: string;
  times: number;
  last_seen: string;
  last_release?: string;
}

export interface LiveState {
  /** Whether the stream is currently connected. */
  connected: boolean;
  /**
   * Issues that appeared or came back since the list was last loaded, by id.
   *
   * A count of ids rather than a count of events: the same issue breaking
   * forty more times is one row the reader has not seen, and a banner saying
   * "40 new issues" for it would be a lie that costs trust the first time
   * somebody checks.
   */
  arrived: LiveIssue[];
}

/**
 * useLiveIssues watches a project and collects what the reader has not seen.
 *
 * It deliberately does not touch the list. Reordering rows under somebody who
 * is reading them — during an incident, which is when this feed matters — moves
 * the line they were about to click. So the arrivals pile up here and the
 * screen offers a banner; pressing it is what reloads.
 *
 * `known` is the ids already on screen. It is read through a ref rather than
 * taken as a dependency because it changes on every page load, and rebuilding
 * the EventSource for that would drop the connection every time somebody
 * pressed "load more".
 */
export function useLiveIssues(projectID: number, known: Set<number>): LiveState {
  const [state, setState] = useState<LiveState>({ connected: false, arrived: [] });
  const knownRef = useRef(known);
  knownRef.current = known;

  // A stable description of what is on screen. The Set itself is a new object
  // on every render, so it cannot be a dependency; the ids it holds can.
  const knownKey = [...known].sort((left, right) => left - right).join(",");

  // An arrival that is now on screen is no longer an arrival.
  //
  // Without this the banner survives the reload it asked for: the rows come
  // back with the new issue among them and the button still offers to show
  // it. Pruning here rather than clearing on the click covers every way the
  // list can be reloaded — a filter change, a status tab, the back button —
  // and not just the one that goes through the banner.
  useEffect(() => {
    setState((current) => {
      const remaining = current.arrived.filter(
        (issue) => !knownRef.current.has(issue.issue_id),
      );
      return remaining.length === current.arrived.length
        ? current
        : { ...current, arrived: remaining };
    });
  }, [knownKey]);

  useEffect(() => {
    // EventSource is present in every browser the panel supports, but not in
    // a server-rendered or scripted context — and a panel that throws on load
    // is worse than one without a live feed.
    if (typeof EventSource === "undefined") return;

    const source = new EventSource(
      `/api/v1/projects/${projectID}/events/stream`,
    );

    const collect = (event: MessageEvent<string>) => {
      let issue: LiveIssue;
      try {
        issue = JSON.parse(event.data) as LiveIssue;
      } catch {
        // A frame this build cannot parse is a newer server talking to an
        // older panel. Ignoring it keeps the rest of the feed working, which
        // is the same rule the ingest side follows for unknown item types.
        return;
      }
      setState((current) => {
        if (knownRef.current.has(issue.issue_id)) return current;
        if (current.arrived.some((seen) => seen.issue_id === issue.issue_id)) {
          return current;
        }
        return { ...current, arrived: [...current.arrived, issue] };
      });
    };

    source.addEventListener("issue.new", collect as EventListener);
    source.addEventListener("issue.regressed", collect as EventListener);
    // issue.updated is deliberately not collected. Another occurrence of
    // something already on the screen is not news the reader has to act on,
    // and counting it would turn the banner into a hit counter.

    source.onopen = () => setState((current) => ({ ...current, connected: true }));
    source.onerror = () => {
      // EventSource retries on its own; this only reflects that it is
      // currently down, so the screen can stop claiming to be live.
      setState((current) => ({ ...current, connected: false }));
    };

    return () => {
      source.close();
      setState({ connected: false, arrived: [] });
    };
  }, [projectID]);

  return state;
}
