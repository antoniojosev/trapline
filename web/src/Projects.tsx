import { useEffect, useState } from "react";
import { api, type Project } from "./api";
import { issuesPath, settingsPath } from "./routes";
import { Button, Copy, Field, Header, Notice } from "./ui";

export function Projects({ onSignedOut }: { onSignedOut: () => void }) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  async function refresh() {
    try {
      setProjects(await api.listProjects());
      setError("");
    } catch (err) {
      if (err instanceof Error && "isUnauthorized" in err && err.isUnauthorized) {
        onSignedOut();
        return;
      }
      setError(err instanceof Error ? err.message : "could not load projects");
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    void refresh();
    // refresh is stable for the component's lifetime; re-running on every
    // render would poll the API continuously.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function act(action: () => Promise<unknown>) {
    setError("");
    try {
      await action();
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "something went wrong");
    }
  }

  return (
    <div className="mx-auto max-w-3xl space-y-8 p-6">
      <Header onSignedOut={onSignedOut}>Projects</Header>

      <form
        className="flex items-end gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          if (!name.trim()) return;
          void act(async () => {
            await api.createProject(name);
            setName("");
          });
        }}
      >
        <div className="flex-1">
          <Field
            label="New project"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="my-app"
          />
        </div>
        <Button type="submit">Create</Button>
      </form>

      <Notice>{error}</Notice>

      {loading ? (
        <p className="text-sm text-muted">loading…</p>
      ) : projects.length === 0 ? (
        <p className="text-sm text-muted">
          No projects yet. Create one and point an SDK's DSN at it — that is the
          whole migration.
        </p>
      ) : (
        <ul className="space-y-6">
          {projects.map((project) => (
            <li key={project.id} className="space-y-3 border-b border-line pb-6">
              <div className="flex items-baseline justify-between">
                <h2 className="font-medium">
                  {project.name}{" "}
                  {/* The slug is derived from the name, not chosen, so this
                      screen is the only place anyone can find out what it is —
                      and it is what a deploy pipeline has to be told. */}
                  <span className="font-mono text-xs text-muted">
                    {project.slug}
                  </span>{" "}
                  <span className="text-xs text-muted">#{project.id}</span>
                </h2>
                <div className="flex gap-2">
                  <Button variant="quiet" href={issuesPath(project.id)}>
                    Issues
                  </Button>
                  <Button variant="quiet" href={settingsPath(project.id)}>
                    Settings
                  </Button>
                  <Button
                    variant="quiet"
                    onClick={() => void act(() => api.rotateKey(project.id))}
                  >
                    Rotate key
                  </Button>
                  <Button
                    variant="danger"
                    onClick={() => void act(() => api.deleteProject(project.id))}
                  >
                    Delete
                  </Button>
                </div>
              </div>

              <div className="space-y-2">
                {project.keys.map((key, index) => (
                  <div key={key.public_key} className="flex items-start gap-3">
                    <Copy value={key.dsn} />
                    {/* During a rotation a project has two live keys. Saying
                        which is which is the difference between finishing the
                        rotation and revoking the one still in production. */}
                    {project.keys.length > 1 && (
                      <span className="shrink-0 pt-1 text-xs text-muted">
                        {index === 0 ? "in use" : "new"}
                      </span>
                    )}
                    {project.keys.length > 1 && index === 0 && (
                      <button
                        type="button"
                        className="shrink-0 pt-1 text-xs text-red-600 hover:underline"
                        onClick={() =>
                          void act(() => api.revokeKey(key.public_key))
                        }
                      >
                        revoke
                      </button>
                    )}
                  </div>
                ))}
              </div>

              {project.keys.length > 1 && (
                <p className="text-xs text-muted">
                  Both keys work. Deploy the new one, then revoke the old.
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
