// The one admin this gate creates, shared by the two halves of the run.
//
// Two halves because release health cannot be read in the same breath as it is
// reported: the counters live in a bounded window in memory and are written
// down on a flush or on a clean shutdown (ADR 008). So `smoke.spec.ts` reports
// the sessions, `scripts/ui-smoke.sh` restarts the server, and
// `health.spec.ts` opens the panel again and reads what survived — which is
// the property worth checking, and the one a sixty-second sleep would have
// checked far more slowly and far less honestly.
//
// The second half starts from a clean browser, so it signs in rather than
// inheriting a cookie. That is not overhead: it is the only part of this suite
// that proves the session survives a server restart, because the session is a
// hash in the database and not state in the process.

export const ADMIN = "antonio";
export const PASSWORD = "una contraseña larga y buena";
export const PROJECT = "ui-smoke";
