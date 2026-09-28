import { useState } from "react";
import { api } from "./api";
import { Button, Field, Notice } from "./ui";

/**
 * Auth is both the first-run setup and the login form.
 *
 * One component for both because they differ in exactly two ways — the wording
 * and which endpoint they call — and the flows that follow are identical. Two
 * components would be two places to keep the password rules consistent.
 */
export function Auth({
  needsSetup,
  onAuthenticated,
}: {
  needsSetup: boolean;
  onAuthenticated: () => void;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setError("");
    setBusy(true);
    try {
      if (needsSetup) {
        // Setup logs the new admin straight in: making someone retype the
        // password they chose two seconds ago is friction with no security
        // value.
        await api.setup(username, password);
      } else {
        await api.login(username, password);
      }
      onAuthenticated();
    } catch (err) {
      setError(err instanceof Error ? err.message : "something went wrong");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-full items-center justify-center p-6">
      <form onSubmit={submit} className="w-full max-w-sm space-y-4">
        <div>
          <h1 className="text-xl font-semibold">
            {needsSetup ? "Create your admin account" : "Sign in"}
          </h1>
          <p className="mt-1 text-sm text-muted">
            {needsSetup
              ? "This runs once. There is no open registration afterwards."
              : "trapline"}
          </p>
        </div>

        <Field
          label="Username"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          autoComplete="username"
          autoFocus
          required
        />
        <Field
          label="Password"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete={needsSetup ? "new-password" : "current-password"}
          required
        />
        {needsSetup && (
          <p className="text-xs text-muted">
            At least 12 characters. Length is the only rule — a passphrase beats
            a short password with a symbol bolted on.
          </p>
        )}

        <Notice>{error}</Notice>

        <Button type="submit" disabled={busy}>
          {busy ? "working…" : needsSetup ? "Create account" : "Sign in"}
        </Button>
      </form>
    </div>
  );
}
