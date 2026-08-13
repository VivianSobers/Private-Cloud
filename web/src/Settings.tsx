import { useCallback, useEffect, useState } from "react";

import {
  ApiError,
  api,
  formatDate,
  type AppPassword,
  type Credential,
  type Device,
  type Me,
  type Session,
} from "./api";
import { describeError, register } from "./webauthn";

interface Props {
  me: Me;
  onChanged: () => Promise<void> | void;
  onCodes: (codes: string[]) => void;
}

export function Settings({ me, onChanged, onCodes }: Props) {
  const [credentials, setCredentials] = useState<Credential[]>([]);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const [c, s] = await Promise.all([api.credentials(), api.sessions()]);
      setCredentials(c.credentials);
      setSessions(s.sessions);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function guard(fn: () => Promise<unknown>) {
    setBusy(true);
    setError(null);
    try {
      await fn();
      await load();
      await onChanged();
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="stack">
      {error && <div className="banner error">{error}</div>}

      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Passkeys</h2>
        <p className="muted small" style={{ margin: 0 }}>
          Register one per device you actually use. There is no password on this
          server, so several passkeys is the cheapest insurance against losing
          access — and the API refuses to delete your last one.
        </p>

        {credentials.length === 0 ? (
          <p className="muted">No passkeys yet.</p>
        ) : (
          <table className="listing">
            <thead>
              <tr>
                <th>Name</th>
                <th className="when" style={{ textAlign: "right" }}>
                  Added
                </th>
                <th className="when" style={{ textAlign: "right" }}>
                  Last used
                </th>
                <th />
              </tr>
            </thead>
            <tbody>
              {credentials.map((c) => (
                <tr key={c.id}>
                  <td className="name">{c.name}</td>
                  <td className="when">{formatDate(c.created_at)}</td>
                  <td className="when">{c.last_used_at ? formatDate(c.last_used_at) : "never"}</td>
                  <td className="actions">
                    <button
                      className="link danger"
                      disabled={busy || credentials.length <= 1}
                      title={credentials.length <= 1 ? "Register another passkey before removing this one" : ""}
                      onClick={() =>
                        void guard(async () => {
                          if (!window.confirm(`Remove the passkey "${c.name}"?`)) return;
                          await api.deleteCredential(c.id);
                        })
                      }
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}

        <div className="row">
          <button
            disabled={busy}
            onClick={() =>
              void guard(async () => {
                const name = window.prompt("Name this passkey", "another device");
                if (!name) return;
                await register(me.user.username, name);
              })
            }
          >
            Add a passkey
          </button>
        </div>
      </section>

      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Recovery codes</h2>
        <p className="muted small" style={{ margin: 0 }}>
          {me.remaining_recovery_codes} unused. Each one is good for a single
          15-minute session that can do nothing but enrol a passkey. Generating
          a new set invalidates every existing code.
        </p>
        <div className="row">
          <button
            disabled={busy}
            onClick={() =>
              void guard(async () => {
                if (
                  !window.confirm(
                    "Generate a new set? Every existing recovery code stops working immediately.",
                  )
                )
                  return;
                const res = await api.regenerateRecovery();
                onCodes(res.recovery_codes);
              })
            }
          >
            Generate new codes
          </button>
        </div>
      </section>

      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Signed-in devices</h2>
        <p className="muted small" style={{ margin: 0 }}>
          Sessions are rows on the server, not tokens — revoking one takes effect
          on the very next request rather than whenever it would have expired.
        </p>
        <table className="listing">
          <thead>
            <tr>
              <th>Device</th>
              <th className="when" style={{ textAlign: "right" }}>
                Last seen
              </th>
              <th />
            </tr>
          </thead>
          <tbody>
            {sessions.map((s) => (
              <tr key={s.id}>
                <td className="name">
                  {s.user_agent || "unknown device"}
                  {s.current && <span className="muted small"> · this device</span>}
                  {s.kind !== "web" && <span className="muted small"> · {s.kind}</span>}
                </td>
                <td className="when">{formatDate(s.last_seen_at)}</td>
                <td className="actions">
                  <button
                    className="link danger"
                    disabled={busy}
                    onClick={() =>
                      void guard(async () => {
                        await api.revokeSession(s.id);
                      })
                    }
                  >
                    {s.current ? "Sign out" : "Revoke"}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      <SyncDevices />

      <AppPasswords />

      {me.user.is_admin && <AdminSection />}
    </div>
  );
}

/**
 * SyncDevices lists the machines running the sync client — distinct from the
 * sessions above. A session answers "what is signed in"; a device answers "which
 * of my machines is syncing", carries a name the user chose, and can be renamed
 * or revoked. Revoking cuts a lost laptop off on its next request.
 */
function SyncDevices() {
  const [devices, setDevices] = useState<Device[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const r = await api.devices();
      setDevices(r.devices);
      setError(null);
    } catch (err) {
      // A server without the Phase 6 device endpoints simply shows no section.
      if (err instanceof ApiError && err.status === 404) {
        setDevices([]);
        return;
      }
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function guard(fn: () => Promise<unknown>) {
    setBusy(true);
    setError(null);
    try {
      await fn();
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function rename(d: Device) {
    const name = window.prompt("Rename this device", d.name);
    if (name === null) return;
    const trimmed = name.trim();
    if (trimmed === "" || trimmed === d.name) return;
    await guard(() => api.renameDevice(d.id, trimmed));
  }

  // Nothing to show and nothing broken: stay quiet rather than render an empty card.
  if (devices !== null && devices.length === 0 && !error) return null;

  return (
    <section className="card stack">
      <h2 style={{ margin: 0, fontSize: "1rem" }}>Sync devices</h2>
      <p className="muted small" style={{ margin: 0 }}>
        Machines running the sync client. Revoking one stops it syncing on its
        very next request — the answer to a lost laptop, not an eventual one.
      </p>
      {error && <div className="banner error">{error}</div>}
      {devices === null ? (
        <p className="muted small" style={{ margin: 0 }}>
          Loading…
        </p>
      ) : (
        <table className="listing">
          <thead>
            <tr>
              <th>Device</th>
              <th className="when" style={{ textAlign: "right" }}>
                Last seen
              </th>
              <th />
            </tr>
          </thead>
          <tbody>
            {devices.map((d) => (
              <tr key={d.id}>
                <td className="name">
                  {d.name}
                  {d.current && <span className="muted small"> · this device</span>}
                  {d.platform && <span className="muted small"> · {d.platform}</span>}
                  {d.app_version && <span className="muted small"> · v{d.app_version}</span>}
                  {d.has_push && <span className="muted small"> · push on</span>}
                </td>
                <td className="when">{formatDate(d.last_seen_at)}</td>
                <td className="actions">
                  <button className="link" disabled={busy} onClick={() => void rename(d)}>
                    Rename
                  </button>
                  {!d.current && (
                    <button
                      className="link danger"
                      disabled={busy}
                      onClick={() => void guard(() => api.revokeDevice(d.id))}
                    >
                      Revoke
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

/**
 * AppPasswords manages credentials for clients that cannot run a WebAuthn
 * ceremony — which in practice means WebDAV mounts.
 */
function AppPasswords() {
  const [list, setList] = useState<AppPassword[]>([]);
  const [fresh, setFresh] = useState<{ name: string; password: string } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      setList((await api.appPasswords()).app_passwords);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const davUrl = `${window.location.origin}/dav/`;

  return (
    <section className="card stack">
      <h2 style={{ margin: 0, fontSize: "1rem" }}>App passwords &amp; WebDAV</h2>
      <p className="muted small" style={{ margin: 0 }}>
        Mount this server as a network drive at <code>{davUrl}</code>. A
        filesystem driver cannot run a passkey prompt, so those clients need an
        app password instead — one per client, individually revocable. Sign in
        with your username and the app password.
      </p>
      <p className="muted small" style={{ margin: 0 }}>
        An app password reaches <code>/dav</code> and nothing else. It cannot
        call the API, so a leaked one cannot enrol a passkey, read your recovery
        codes or change how you sign in.
      </p>

      {error && <div className="banner error">{error}</div>}

      {fresh && (
        <div className="banner warn stack">
          <strong>App password for “{fresh.name}” — shown once</strong>
          <pre className="codes">{fresh.password}</pre>
          <div className="row">
            <button onClick={() => void navigator.clipboard.writeText(fresh.password).catch(() => {})}>
              Copy
            </button>
            <button className="primary" onClick={() => setFresh(null)}>
              Done
            </button>
          </div>
        </div>
      )}

      {list.length > 0 && (
        <table className="listing">
          <thead>
            <tr>
              <th>Client</th>
              <th className="when" style={{ textAlign: "right" }}>
                Last used
              </th>
              <th />
            </tr>
          </thead>
          <tbody>
            {list.map((a) => (
              <tr key={a.id}>
                <td className="name">
                  {a.name}
                  {a.expires_at && (
                    <span className="muted small"> · expires {formatDate(a.expires_at)}</span>
                  )}
                </td>
                <td className="when">{a.last_used_at ? formatDate(a.last_used_at) : "never"}</td>
                <td className="actions">
                  <button
                    className="link danger"
                    disabled={busy}
                    onClick={async () => {
                      if (!window.confirm(`Revoke "${a.name}"? Any client using it stops working immediately.`))
                        return;
                      setBusy(true);
                      try {
                        await api.revokeAppPassword(a.id);
                        await load();
                      } catch (err) {
                        setError(err instanceof ApiError ? err.message : String(err));
                      } finally {
                        setBusy(false);
                      }
                    }}
                  >
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <div className="row">
        <button
          disabled={busy}
          onClick={async () => {
            const name = window.prompt("What client is this for?", "Finder on my laptop");
            if (!name) return;
            setBusy(true);
            try {
              const res = await api.createAppPassword(name);
              setFresh({ name, password: res.password });
              await load();
            } catch (err) {
              setError(err instanceof ApiError ? err.message : String(err));
            } finally {
              setBusy(false);
            }
          }}
        >
          Create an app password
        </button>
      </div>
    </section>
  );
}

function AdminSection() {
  const [result, setResult] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  return (
    <section className="card stack">
      <h2 style={{ margin: 0, fontSize: "1rem" }}>Storage audit</h2>
      <p className="muted small" style={{ margin: 0 }}>
        Compares what the database believes against what is on disk. Read-only
        unless you ask it to repair, and repair only removes content nothing
        references — it never deletes a record of a file you still expect to
        exist.
      </p>
      <div className="row">
        <button
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            try {
              setResult(JSON.stringify(await api.fsck(false), null, 2));
            } catch (err) {
              setResult(err instanceof ApiError ? err.message : String(err));
            } finally {
              setBusy(false);
            }
          }}
        >
          Run audit
        </button>
      </div>
      {result && <pre className="codes">{result}</pre>}
    </section>
  );
}
