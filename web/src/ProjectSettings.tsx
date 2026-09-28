// The project's ingest profile: which categories it accepts, its spike
// ceiling and how long each category is kept.
//
// The API returns what someone chose and what applies by default as two
// separate things, and this form keeps them separate all the way to the save.
// Merging them into one value would read better and destroy the only fact that
// matters here: a project that never chose a retention has not chosen 90 days,
// it has chosen to follow whatever this installation decides retention should
// be. Save the merged view once and raising a default later reaches nobody.

import { useCallback, useEffect, useState } from "react";
import {
  ApiError,
  api,
  type ProjectConfig,
  type ProjectConfigChanges,
} from "./api";
import { Link, issuesPath, projectsPath } from "./routes";
import { Button, Header, Input, Notice } from "./ui";

// Inherited is null rather than a copy of the default, everywhere in here.
type Draft = {
  categories: string[] | null;
  rateLimit: number | null;
  retention: Record<string, number | null>;
};

function draftOf(config: ProjectConfig): Draft {
  const retention: Record<string, number | null> = {};
  for (const category of config.available_categories) {
    const days = config.retention_days[category];
    retention[category] = days ? days : null;
  }
  return {
    categories: config.enabled_categories,
    // Zero is how this API spells "unset" for the numbers; null is how this
    // form spells it, so the two never have to be told apart further in.
    rateLimit: config.rate_limit_per_minute || null,
    retention,
  };
}

function sameList(a: string[] | null, b: string[] | null): boolean {
  if (a === null || b === null) return a === b;
  return a.length === b.length && a.every((value, index) => value === b[index]);
}

/**
 * changesBetween is what actually gets sent: the fields the operator touched
 * and nothing else.
 *
 * Sending the whole form back would overwrite every inherited value with the
 * default that happened to be showing, which is the same freezing failure by a
 * quieter route — the project would look unchanged and would have stopped
 * inheriting.
 */
function changesBetween(saved: Draft, draft: Draft): ProjectConfigChanges {
  const changes: ProjectConfigChanges = {};

  if (!sameList(saved.categories, draft.categories)) {
    changes.enabled_categories = draft.categories;
  }
  if (saved.rateLimit !== draft.rateLimit) {
    changes.rate_limit_per_minute = draft.rateLimit;
  }

  // Retention is one field on the wire, so a single changed category means
  // sending every category that is explicitly set — and only those.
  const categories = Object.keys(draft.retention);
  if (categories.some((c) => saved.retention[c] !== draft.retention[c])) {
    const explicit: Record<string, number> = {};
    for (const category of categories) {
      const days = draft.retention[category];
      if (days) explicit[category] = days;
    }
    changes.retention_days =
      Object.keys(explicit).length > 0 ? explicit : null;
  }

  return changes;
}

// Said plainly, because a switched-off category answers an SDK with a
// rate-limit response, and whoever is reading SDK logs will see it as an error
// unless someone told them it is the mechanism (ADR 005).
function refusalNote(off: string[]): string {
  return `${off.join(", ")} ${off.length === 1 ? "is" : "are"} refused with a rate-limit response, so SDKs stop sending ${off.length === 1 ? "it" : "them"} instead of retrying.`;
}

/** Inherited marks a value that is not this project's decision. */
function Inherited() {
  return <span className="text-xs text-muted">(default)</span>;
}

export function ProjectSettings({
  projectID,
  onSignedOut,
}: {
  projectID: number;
  onSignedOut: () => void;
}) {
  const [config, setConfig] = useState<ProjectConfig | null>(null);
  // saved is what the server last confirmed; draft is what is on screen. The
  // difference between them is the request.
  const [saved, setSaved] = useState<Draft | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [projectName, setProjectName] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [saving, setSaving] = useState(false);
  const [loading, setLoading] = useState(true);

  const adopt = useCallback((loaded: ProjectConfig) => {
    setConfig(loaded);
    setSaved(draftOf(loaded));
    setDraft(draftOf(loaded));
  }, []);

  useEffect(() => {
    let current = true;
    (async () => {
      try {
        const [loaded, project] = await Promise.all([
          api.getProjectConfig(projectID),
          api.getProject(projectID),
        ]);
        if (!current) return;
        adopt(loaded);
        setProjectName(project.name);
        setError("");
      } catch (err) {
        if (!current) return;
        if (err instanceof ApiError && err.isUnauthorized) {
          onSignedOut();
          return;
        }
        setError(
          err instanceof Error ? err.message : "could not load the settings",
        );
      } finally {
        if (current) setLoading(false);
      }
    })();
    return () => {
      current = false;
    };
  }, [projectID, adopt, onSignedOut]);

  if (loading) return <p className="p-6 text-sm text-muted">loading…</p>;
  if (!config || !saved || !draft) {
    return (
      <div className="mx-auto max-w-3xl space-y-6 p-6">
        <Header onSignedOut={onSignedOut}>Settings</Header>
        <Notice>{error || "could not load the settings"}</Notice>
      </div>
    );
  }

  const effectiveCategories =
    draft.categories ?? config.defaults.enabled_categories;
  const off = config.available_categories.filter(
    (category) => !effectiveCategories.includes(category),
  );
  const changes = changesBetween(saved, draft);
  const dirty = Object.keys(changes).length > 0;

  function toggle(category: string, on: boolean) {
    // The first click on an inherited list has to write the inherited profile
    // down: from here on it is this project's own list, which is exactly what
    // the operator just said by touching it.
    const base = draft?.categories ?? config?.defaults.enabled_categories ?? [];
    const next = on
      ? [...base, category]
      : base.filter((value) => value !== category);
    // Kept in the engine's order rather than click order, so two operators who
    // enabled the same categories produce the same stored value.
    const ordered = (config?.available_categories ?? []).filter((value) =>
      next.includes(value),
    );
    setDraft((previous) =>
      previous ? { ...previous, categories: ordered } : previous,
    );
  }

  async function save() {
    if (!dirty || saving) return;
    setSaving(true);
    setError("");
    setNotice("");
    try {
      adopt(await api.setProjectConfig(projectID, changes));
      setNotice("Saved. It applies to the next event, not to the next restart.");
    } catch (err) {
      if (err instanceof ApiError && err.isUnauthorized) {
        onSignedOut();
        return;
      }
      setError(err instanceof Error ? err.message : "could not save");
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="mx-auto max-w-3xl space-y-8 p-6">
      <Header onSignedOut={onSignedOut}>
        <Link
          to={projectsPath}
          className="font-normal text-muted hover:text-accent"
        >
          Projects
        </Link>
        <span className="px-2 text-muted">/</span>
        <Link
          to={issuesPath(projectID)}
          className="font-normal text-muted hover:text-accent"
        >
          {projectName || `#${projectID}`}
        </Link>
        <span className="px-2 text-muted">/</span>
        Settings
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

      <section className="space-y-3">
        <div className="flex items-baseline gap-2">
          <h2 className="font-medium">Categories</h2>
          {draft.categories === null ? <Inherited /> : null}
        </div>
        <p className="text-sm text-muted">
          What this project accepts. Everything else is refused before it is
          decoded, which is what makes a subsystem you do not use cost nothing.
        </p>

        <ul className="space-y-1">
          {config.available_categories.map((category) => (
            <li key={category}>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  className="accent-accent"
                  checked={effectiveCategories.includes(category)}
                  onChange={(event) => toggle(category, event.target.checked)}
                />
                <span className="font-mono">{category}</span>
              </label>
            </li>
          ))}
        </ul>

        {off.length > 0 ? (
          <p className="text-xs text-muted">{refusalNote(off)}</p>
        ) : null}

        {draft.categories === null ? (
          <p className="text-xs text-muted">
            Inheriting the default profile:{" "}
            {config.defaults.enabled_categories.join(", ") || "nothing"}.
          </p>
        ) : (
          <button
            type="button"
            className="text-xs text-muted hover:text-accent"
            onClick={() =>
              setDraft((previous) =>
                previous ? { ...previous, categories: null } : previous,
              )
            }
          >
            Use the default profile
          </button>
        )}
      </section>

      <section className="space-y-3">
        <div className="flex items-baseline gap-2">
          <h2 className="font-medium">Rate limit</h2>
          {draft.rateLimit === null ? <Inherited /> : null}
        </div>
        <p className="text-sm text-muted">
          Spike protection: the ceiling per category, per minute. The usual
          cause of a spike is a retry loop reporting an error per iteration, so
          this only has to sit comfortably above real traffic.
        </p>
        <div className="flex items-baseline gap-3">
          <div className="w-40">
            <Input
              type="number"
              min={0}
              inputMode="numeric"
              aria-label="Rate limit per minute"
              placeholder={String(config.defaults.rate_limit_per_minute)}
              value={draft.rateLimit === null ? "" : String(draft.rateLimit)}
              onChange={(event) => {
                const raw = event.target.value.trim();
                // An empty field is how you say "inherit" — the same gesture
                // as never having filled it in.
                const value = raw === "" ? null : Number(raw);
                setDraft((previous) =>
                  previous
                    ? {
                        ...previous,
                        rateLimit:
                          value === null || Number.isNaN(value) || value <= 0
                            ? null
                            : value,
                      }
                    : previous,
                );
              }}
            />
          </div>
          <span className="text-sm text-muted">
            events/minute ·{" "}
            {Math.floor(
              (draft.rateLimit ?? config.defaults.rate_limit_per_minute) / 60,
            )}
            /second
          </span>
        </div>
        <p className="text-xs text-muted">Leave it empty to inherit.</p>
      </section>

      <section className="space-y-3">
        <h2 className="font-medium">Retention</h2>
        <p className="text-sm text-muted">
          How long each category's events are kept. Empty inherits.
        </p>
        <ul className="space-y-2">
          {config.available_categories.map((category) => {
            const days = draft.retention[category] ?? null;
            return (
              <li key={category} className="flex items-baseline gap-3">
                <span className="w-28 shrink-0 font-mono text-sm">
                  {category}
                </span>
                <div className="w-28">
                  <Input
                    type="number"
                    min={0}
                    inputMode="numeric"
                    aria-label={`Retention for ${category} in days`}
                    placeholder={String(
                      config.defaults.retention_days[category] ?? 0,
                    )}
                    value={days === null ? "" : String(days)}
                    onChange={(event) => {
                      const raw = event.target.value.trim();
                      const value = raw === "" ? null : Number(raw);
                      setDraft((previous) =>
                        previous
                          ? {
                              ...previous,
                              retention: {
                                ...previous.retention,
                                [category]:
                                  value === null ||
                                  Number.isNaN(value) ||
                                  value <= 0
                                    ? null
                                    : value,
                              },
                            }
                          : previous,
                      );
                    }}
                  />
                </div>
                <span className="text-sm text-muted">days</span>
                {days === null ? <Inherited /> : null}
              </li>
            );
          })}
        </ul>
      </section>

      <div className="flex items-center gap-3 border-t border-line pt-4">
        <Button onClick={() => void save()} disabled={!dirty || saving}>
          {saving ? "Saving…" : "Save"}
        </Button>
        {dirty ? (
          <span className="text-xs text-muted">
            Only what you changed is sent; the rest keeps inheriting.
          </span>
        ) : null}
      </div>
    </div>
  );
}
