import { useCallback, useEffect, useState } from "react";

import { ApiError, api, type Me } from "./api";
import { SignIn } from "./SignIn";
import { Admin } from "./Admin";
import { Ask } from "./Ask";
import { Browser } from "./Browser";
import { Offline } from "./Offline";
import { People } from "./People";
import { Photos } from "./Photos";
import { Settings } from "./Settings";
import { SharedWithMe } from "./SharedWithMe";
import { ShareTarget } from "./ShareTarget";
import { RecoveryCodes } from "./RecoveryCodes";
import { shareArrived } from "./share";

type View = "files" | "photos" | "people" | "ask" | "shared" | "offline" | "admin" | "settings";

export function App() {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);
  const [view, setView] = useState<View>("files");
  // A folder somebody else granted, opened from "Shared with me". Undefined is
  // the ordinary case: the browser starts at the user's own root.
  const [openFolder, setOpenFolder] = useState<string | undefined>(undefined);

  // Codes are shown exactly once, right after they are issued. Held here rather
  // than in the component that created them so navigating away cannot make them
  // vanish before they have been written down.
  const [freshCodes, setFreshCodes] = useState<string[] | null>(null);

  // Arrived from the OS share sheet: the service worker parked the files and
  // redirected here with ?share=inbox. Read once, at mount — the flag is a
  // one-shot fact about how this page load started, not a route.
  const [sharing, setSharing] = useState(() =>
    typeof window !== "undefined" ? shareArrived(window.location.search) : false,
  );

  // Drop the marker so a refresh, or a later back-button, does not reopen a
  // share that has already been dealt with.
  const dismissShare = useCallback(() => {
    setSharing(false);
    if (typeof window !== "undefined") {
      window.history.replaceState(null, "", window.location.pathname);
    }
  }, []);

  const finishShare = useCallback(() => {
    dismissShare();
    setOpenFolder(undefined);
    setView("files");
  }, [dismissShare]);

  // Navigating anywhere by hand also abandons the share screen — otherwise the
  // nav would appear to do nothing while the share sat on top of it.
  const go = useCallback(
    (v: View) => {
      dismissShare();
      setView(v);
    },
    [dismissShare],
  );

  const refresh = useCallback(async () => {
    try {
      setMe(await api.me());
    } catch (err) {
      // 401 is the normal signed-out state, not a failure worth reporting.
      if (!(err instanceof ApiError) || err.status !== 401) {
        console.error("could not load session", err);
      }
      setMe(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  if (loading) {
    return (
      <div className="app">
        <p className="muted">Loading…</p>
      </div>
    );
  }

  if (!me) {
    return <SignIn onSignedIn={refresh} onCodes={setFreshCodes} codes={freshCodes} />;
  }

  // A recovery session can do exactly one thing: enrol a passkey. Rendering the
  // file browser would be a lie — every request it made would come back 403.
  if (me.session_kind === "recovery") {
    return (
      <SignIn
        recoveryFor={me.user.username}
        onSignedIn={refresh}
        onCodes={setFreshCodes}
        codes={freshCodes}
      />
    );
  }

  return (
    <div className="app">
      <header className="top">
        <h1>private cloud</h1>
        <nav className="row small">
          <button
            className="link"
            onClick={() => {
              // "Files" always means your own files, even if the last thing open
              // was a folder somebody shared with you.
              setOpenFolder(undefined);
              go("files");
            }}
            aria-current={view === "files"}
          >
            Files
          </button>
          <button className="link" onClick={() => go("photos")} aria-current={view === "photos"}>
            Photos
          </button>
          <button className="link" onClick={() => go("people")} aria-current={view === "people"}>
            People
          </button>
          <button className="link" onClick={() => go("ask")} aria-current={view === "ask"}>
            Ask
          </button>
          <button className="link" onClick={() => go("shared")} aria-current={view === "shared"}>
            Shared
          </button>
          <button className="link" onClick={() => go("offline")} aria-current={view === "offline"}>
            Offline
          </button>
          {me.user.is_admin && (
            <button className="link" onClick={() => go("admin")} aria-current={view === "admin"}>
              Admin
            </button>
          )}
          <button className="link" onClick={() => go("settings")} aria-current={view === "settings"}>
            Settings
          </button>
        </nav>
        <span className="muted small">
          {me.user.display_name || me.user.username}
          {me.user.is_admin ? " (admin)" : ""}
        </span>
        <button
          onClick={async () => {
            await api.logout();
            await refresh();
          }}
        >
          Sign out
        </button>
      </header>

      {me.remaining_recovery_codes === 0 && (
        <div className="banner warn">
          <strong>No recovery codes left.</strong> Without one, losing every
          passkey means losing access to this server — the only way back in
          would be a shell on the machine itself.{" "}
          <button
            className="link"
            onClick={async () => {
              const res = await api.regenerateRecovery();
              setFreshCodes(res.recovery_codes);
              await refresh();
            }}
          >
            Generate a new set
          </button>
        </div>
      )}

      {freshCodes && <RecoveryCodes codes={freshCodes} onDismiss={() => setFreshCodes(null)} />}

      {sharing ? (
        <ShareTarget onDone={finishShare} />
      ) : view === "files" ? (
        // The key remounts the browser when a different shared folder is opened,
        // so its initial load runs again instead of the prop being ignored by an
        // already-mounted component sitting on somebody else's folder.
        <Browser key={openFolder ?? "own-root"} initialFolderId={openFolder} />
      ) : view === "photos" ? (
        <Photos />
      ) : view === "people" ? (
        <People />
      ) : view === "ask" ? (
        <Ask />
      ) : view === "shared" ? (
        <SharedWithMe
          onOpenFolder={(id) => {
            setOpenFolder(id);
            setView("files");
          }}
        />
      ) : view === "offline" ? (
        <Offline />
      ) : view === "admin" && me.user.is_admin ? (
        <Admin />
      ) : view === "admin" ? (
        <Browser />
      ) : (
        <Settings me={me} onChanged={refresh} onCodes={setFreshCodes} />
      )}
    </div>
  );
}
