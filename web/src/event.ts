// The stored event payload.
//
// The API returns the whole scrubbed event rather than a summary, so these
// types mirror what the server writes (internal/sentry/event.go) instead of a
// shape invented here. Every field is optional on purpose: a payload is
// whatever an SDK sent, and a field this build did not get is a field to
// render nothing for, never a reason to fail.

export interface Frame {
  filename?: string;
  abs_path?: string;
  function?: string;
  module?: string;
  lineno?: number;
  colno?: number;
  in_app?: boolean;
  context_line?: string;
  pre_context?: string[];
  post_context?: string[];
  /**
   * The minified location this frame had before symbolication.
   *
   * Never sent by an SDK — the server writes it when a source map resolved
   * the frame (ADR 018). It is the only thing that can be compared against a
   * deployed bundle when somebody suspects the map is wrong or stale, which
   * is why the panel can switch back to it rather than only showing the
   * resolved form.
   */
  raw?: RawFrame;
}

/** What symbolication overwrote: only the fields it replaced. */
export interface RawFrame {
  filename?: string;
  abs_path?: string;
  function?: string;
  module?: string;
  lineno?: number;
  colno?: number;
}

export interface Exception {
  type?: string;
  value?: string;
  module?: string;
  stacktrace?: { frames?: Frame[] } | null;
}

export interface Breadcrumb {
  timestamp?: string;
  type?: string;
  category?: string;
  level?: string;
  message?: string;
  data?: Record<string, unknown>;
}

export interface EventPayload {
  event_id?: string;
  timestamp?: string;
  platform?: string;
  level?: string;
  logger?: string;
  release?: string;
  environment?: string;
  server_name?: string;
  transaction?: string;
  message?: string;
  // The uninterpolated form of the message, when the SDK sent one. It exists
  // for grouping and is never rendered: showing "could not charge user %s" to
  // someone trying to find out which user is the opposite of helping.
  message_template?: string;
  fingerprint?: string[];
  exception?: { values?: Exception[] };
  stacktrace?: { frames?: Frame[] } | null;
  tags?: Record<string, string>;
  user?: Record<string, unknown>;
  request?: Record<string, unknown>;
  contexts?: Record<string, unknown>;
  extra?: Record<string, unknown>;
  breadcrumbs?: Breadcrumb[];
}

/** REDACTED is what the scrubber leaves behind where a value was removed. */
export const REDACTED = "[redacted]";

/** Trace is one stacktrace with the exception it belongs to, if any. */
export interface Trace {
  exception?: Exception;
  frames: Frame[];
}

/**
 * traces returns the stacktraces to render, the raised exception first.
 *
 * The protocol orders an exception chain cause-first and puts the exception
 * that was actually raised last. That last one is what the reader came to
 * see, so the chain is walked backwards and its causes follow underneath.
 */
export function traces(payload: EventPayload): Trace[] {
  const chain = payload.exception?.values ?? [];
  if (chain.length === 0) {
    const frames = payload.stacktrace?.frames ?? [];
    return frames.length > 0 ? [{ frames }] : [];
  }
  return [...chain].reverse().map((exception) => ({
    exception,
    frames: exception.stacktrace?.frames ?? [],
  }));
}

/**
 * callOrder returns a stacktrace's frames with the failing call first.
 *
 * Frames arrive oldest-first — the call that actually failed is the LAST one.
 * Rendering them as they arrive puts the process entry point at the top of
 * the page and buries the line that raised, so the reversal happens once,
 * here, rather than being remembered at every call site.
 */
export function callOrder(frames: Frame[]): Frame[] {
  return [...frames].reverse();
}

/** framePath is the best file path a frame carries. */
export function framePath(frame: Frame): string {
  return frame.abs_path ?? frame.filename ?? "";
}

/**
 * minified is the frame as the browser actually ran it, when this one was
 * resolved from a source map.
 *
 * A Frame rather than a RawFrame, so the same renderer draws both. It carries
 * no source context on purpose: the context lines belong to the original
 * file, and showing them beside a minified position would be captioning one
 * file with another's source.
 */
export function minified(frame: Frame): Frame | null {
  if (!frame.raw) return null;
  return { ...frame.raw, in_app: frame.in_app };
}

/** symbolicated reports whether any frame here was resolved from a map. */
export function symbolicated(frames: Frame[]): boolean {
  return frames.some((frame) => frame.raw !== undefined);
}

/**
 * hasRedaction reports whether anything in the payload was scrubbed.
 *
 * A string scan over a payload already decoded and held in memory, which is
 * cheaper than walking it and is only used to decide whether to explain the
 * marker once at the foot of the page.
 */
export function hasRedaction(payload: EventPayload): boolean {
  return JSON.stringify(payload).includes(REDACTED);
}
