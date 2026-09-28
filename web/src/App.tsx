import { useCallback, useEffect, useState } from "react";
import { ApiError, api } from "./api";
import { Alerts } from "./Alerts";
import { Auth } from "./Auth";
import { Dashboard } from "./Dashboard";
import { IssueDetail } from "./IssueDetail";
import { Issues } from "./Issues";
import { Monitors } from "./Monitors";
import { Performance } from "./Performance";
import { Projects } from "./Projects";
import { ProjectSettings } from "./ProjectSettings";
import { ReleaseDetail } from "./ReleaseDetail";
import { Releases } from "./Releases";
import { TraceDetail } from "./TraceDetail";
import { useRoute } from "./routes";

type State =
  | { kind: "loading" }
  | { kind: "unauthenticated"; needsSetup: boolean }
  | { kind: "authenticated" }
  | { kind: "unreachable"; message: string };

export function App() {
  const [state, setState] = useState<State>({ kind: "loading" });

  const check = useCallback(async () => {
    try {
      // Whether a session already exists is the first question, because a
      // reload must not throw a logged-in operator back to a form.
      await api.me();
      setState({ kind: "authenticated" });
      return;
    } catch (err) {
      if (!(err instanceof ApiError)) {
        setState({
          kind: "unreachable",
          message: err instanceof Error ? err.message : "cannot reach the server",
        });
        return;
      }
      if (!err.isUnauthorized) {
        setState({ kind: "unreachable", message: err.message });
        return;
      }
    }

    try {
      const { needs_setup } = await api.setupStatus();
      setState({ kind: "unauthenticated", needsSetup: needs_setup });
    } catch (err) {
      setState({
        kind: "unreachable",
        message: err instanceof Error ? err.message : "cannot reach the server",
      });
    }
  }, []);

  useEffect(() => {
    void check();
  }, [check]);

  switch (state.kind) {
    case "loading":
      return <p className="p-6 text-sm text-muted">loading…</p>;
    case "unreachable":
      return (
        <div className="p-6">
          <h1 className="font-semibold">Cannot reach the server</h1>
          <p className="mt-1 text-sm text-muted">{state.message}</p>
        </div>
      );
    case "unauthenticated":
      return <Auth needsSetup={state.needsSetup} onAuthenticated={check} />;
    case "authenticated":
      return <Panel onSignedOut={check} />;
  }
}

// Panel is separate from App only because the route hook cannot be called
// from a branch of App's switch.
function Panel({ onSignedOut }: { onSignedOut: () => void }) {
  const route = useRoute();

  switch (route.kind) {
    case "projects":
      return <Projects onSignedOut={onSignedOut} />;
    case "dashboard":
      return <Dashboard projectID={route.projectID} onSignedOut={onSignedOut} />;
    case "performance":
      return (
        <Performance
          projectID={route.projectID}
          transaction={route.transaction}
          onSignedOut={onSignedOut}
        />
      );
    case "trace":
      return (
        <TraceDetail
          projectID={route.projectID}
          traceID={route.traceID}
          onSignedOut={onSignedOut}
        />
      );
    case "releases":
      return <Releases projectID={route.projectID} onSignedOut={onSignedOut} />;
    case "release":
      return (
        <ReleaseDetail
          projectID={route.projectID}
          version={route.version}
          onSignedOut={onSignedOut}
        />
      );
    case "issues":
      return (
        <Issues projectID={route.projectID} onSignedOut={onSignedOut} />
      );
    case "issue":
      return (
        <IssueDetail
          projectID={route.projectID}
          issueID={route.issueID}
          onSignedOut={onSignedOut}
        />
      );
    case "settings":
      return (
        <ProjectSettings projectID={route.projectID} onSignedOut={onSignedOut} />
      );
    case "monitors":
      return <Monitors projectID={route.projectID} onSignedOut={onSignedOut} />;
    case "alerts":
      return <Alerts onSignedOut={onSignedOut} />;
  }
}
