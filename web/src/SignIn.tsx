import { useEffect, useState } from "react";

import { api } from "./api";
import { RecoveryCodes } from "./RecoveryCodes";
import { describeError, isSupported, login, register } from "./webauthn";

interface Props {
  onSignedIn: () => Promise<void> | void;
  onCodes: (codes: string[]) => void;
  codes: string[] | null;
  /** Set when a recovery session is active: the only allowed action is enrolling a passkey. */
  recoveryFor?: string;
}

type Mode = "signin" | "bootstrap" | "recovery";

export function SignIn({ onSignedIn, onCodes, codes, recoveryFor }: Props) {
  const [mode, setMode] = useState<Mode>("signin");
  const [username, setUsername] = useState("");
  const [recoveryCode, setRecoveryCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [bootstrapChecked, setBootstrapChecked] = useState(false);
  const [oidcEnabled, setOidcEnabled] = useState(false);

  useEffect(() => {
    if (recoveryFor) return;
    void api
      .authStatus()
      .then((s) => {
        // An empty users table means nobody has claimed this server yet. Showing
        // the sign-in form first would be a dead end for the one person who
        // actually needs to get in.
        if (s.bootstrap_required) setMode("bootstrap");
        setOidcEnabled(!!s.oidc_enabled);
      })
      .catch(() => {
        /* the API is unreachable; the sign-in attempt will say so properly */
      })
      .finally(() => setBootstrapChecked(true));
  }, [recoveryFor]);

  async function run(fn: () => Promise<void>) {
    setBusy(true);
    setError(null);
    try {
      await fn();
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy(false);
    }
  }

  if (!isSupported()) {
    return (
      <div className="app">
        <div className="card stack">
          <h2>This browser cannot sign in</h2>
          <p>
            This server uses passkeys and has no passwords at all, so a browser
            without WebAuthn has no way to authenticate. Recent Firefox, Chrome,
            Edge and Safari all support it.
          </p>
        </div>
      </div>
    );
  }

  // --- recovery session: enrol a passkey and nothing else --------------------
  if (recoveryFor) {
    return (
      <div className="app">
        <div className="card stack">
          <h2>Add a passkey to {recoveryFor}</h2>
          <p className="muted">
            You are signed in with a recovery code. This session expires in 15
            minutes and can do nothing except register a passkey — that limit is
            what stops a printed code from being as powerful as a passkey.
          </p>
          {error && <div className="banner error">{error}</div>}
          {codes && <RecoveryCodes codes={codes} onDismiss={() => onCodes([])} />}
          <div className="row">
            <button
              className="primary"
              disabled={busy}
              onClick={() =>
                run(async () => {
                  await register(recoveryFor, defaultCredentialName());
                  await onSignedIn();
                })
              }
            >
              {busy ? "Waiting for the authenticator…" : "Register a passkey"}
            </button>
            <button
              disabled={busy}
              onClick={() =>
                run(async () => {
                  await api.logout();
                  await onSignedIn();
                })
              }
            >
              Cancel
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="app">
      <div className="card stack">
        <h2>
          {mode === "bootstrap"
            ? "Claim this server"
            : mode === "recovery"
              ? "Use a recovery code"
              : "Sign in"}
        </h2>

        {mode === "bootstrap" && (
          <p className="muted">
            No account exists yet. The first passkey registered becomes the
            admin. Everyone after that is created on the server with{" "}
            <code>cloudctl user create</code>.
          </p>
        )}

        {error && <div className="banner error">{error}</div>}
        {codes && <RecoveryCodes codes={codes} onDismiss={() => onCodes([])} />}

        <label className="stack">
          <span className="small muted">Username</span>
          <input
            value={username}
            autoComplete="username webauthn"
            autoFocus
            onChange={(e) => setUsername(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && username && !busy) void submit();
            }}
          />
        </label>

        {mode === "recovery" && (
          <label className="stack">
            <span className="small muted">Recovery code</span>
            <input
              value={recoveryCode}
              placeholder="XXXXX-XXXXX-XXXXX-XXXXX"
              spellCheck={false}
              autoCapitalize="characters"
              onChange={(e) => setRecoveryCode(e.target.value)}
            />
          </label>
        )}

        <div className="row">
          <button className="primary" disabled={busy || !username || !bootstrapChecked} onClick={() => void submit()}>
            {busy
              ? "Waiting…"
              : mode === "bootstrap"
                ? "Create admin passkey"
                : mode === "recovery"
                  ? "Redeem code"
                  : "Sign in with passkey"}
          </button>

          {mode === "signin" && oidcEnabled && (
            <button
              disabled={busy}
              onClick={() => {
                // A full navigation, not a fetch: the provider redirect and the
                // callback's Set-Cookie must happen in the top-level browser context.
                window.location.href = "/api/v1/auth/oidc/login";
              }}
            >
              Sign in with SSO
            </button>
          )}
          {mode === "signin" && (
            <button className="link" onClick={() => setMode("recovery")}>
              Lost your passkey?
            </button>
          )}
          {mode === "recovery" && (
            <button className="link" onClick={() => setMode("signin")}>
              Back to sign in
            </button>
          )}
        </div>
      </div>
    </div>
  );

  function submit() {
    return run(async () => {
      if (mode === "bootstrap") {
        const res = (await register(username, defaultCredentialName())) as {
          recovery_codes?: string[];
        };
        // Shown once and never retrievable. Surfacing them before the redirect
        // is the only chance the user gets.
        if (res.recovery_codes?.length) onCodes(res.recovery_codes);
        await onSignedIn();
        return;
      }
      if (mode === "recovery") {
        await api.redeemRecovery(username.trim(), recoveryCode.trim().toUpperCase());
        await onSignedIn();
        return;
      }
      await login(username);
      await onSignedIn();
    });
  }
}

/**
 * defaultCredentialName labels the passkey with the device it lives on, so the
 * settings list reads "Chrome on Windows" rather than four identical rows.
 */
function defaultCredentialName(): string {
  const ua = navigator.userAgent;
  const browser = /Firefox\//.test(ua)
    ? "Firefox"
    : /Edg\//.test(ua)
      ? "Edge"
      : /Chrome\//.test(ua)
        ? "Chrome"
        : /Safari\//.test(ua)
          ? "Safari"
          : "browser";
  const os = /Windows/.test(ua)
    ? "Windows"
    : /Android/.test(ua)
      ? "Android"
      : /iPhone|iPad/.test(ua)
        ? "iOS"
        : /Mac OS X/.test(ua)
          ? "macOS"
          : /Linux/.test(ua)
            ? "Linux"
            : "this device";
  return `${browser} on ${os}`;
}
