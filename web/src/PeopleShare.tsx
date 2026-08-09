import { useCallback, useEffect, useState } from "react";

import { api, ApiError, formatDate, type Grant, type Node, type Role } from "./api";

// The "share with people" panel: grant another user on this server access to a
// node, change their role, or revoke it. Distinct from a public link — this is
// named access for accounts, inheriting down a folder. Reads the Phase 7 grants
// surface (see the API contract) and degrades to a clear notice where the server
// has not implemented it yet.

const ROLES: Role[] = ["viewer", "editor"];

export function PeopleShare({ node }: { node: Node }) {
  const [grants, setGrants] = useState<Grant[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [username, setUsername] = useState("");
  const [role, setRole] = useState<Role>("viewer");
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api
      .grants()
      // Only the direct grants ON this node — inherited ones belong to the
      // ancestor that carries them, and showing them here would imply they can be
      // revoked here, which they cannot.
      .then((r) => setGrants(r.granted.filter((g) => g.node_id === node.id && !g.inherited_from)))
      .catch((e) => setError(describe(e)));
  }, [node.id]);

  useEffect(load, [load]);

  const add = useCallback(async () => {
    const name = username.trim();
    if (!name) return;
    setBusy(true);
    setError(null);
    try {
      await api.grant(node.id, name, role);
      setUsername("");
      load();
    } catch (e) {
      setError(describe(e));
    } finally {
      setBusy(false);
    }
  }, [username, role, node.id, load]);

  const changeRole = useCallback(
    async (g: Grant, next: Role) => {
      try {
        await api.updateGrant(g.id, next);
        load();
      } catch (e) {
        setError(describe(e));
      }
    },
    [load],
  );

  const revoke = useCallback(
    async (g: Grant) => {
      try {
        await api.revokeGrant(g.id);
        load();
      } catch (e) {
        setError(describe(e));
      }
    },
    [load],
  );

  return (
    <div className="stack">
      <strong className="small">People with access</strong>

      <div className="row">
        <input
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          placeholder="username"
          autoComplete="off"
          style={{ flex: 1 }}
          onKeyDown={(e) => e.key === "Enter" && void add()}
        />
        <select value={role} onChange={(e) => setRole(e.target.value as Role)}>
          {ROLES.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
        <button className="primary" disabled={busy} onClick={() => void add()}>
          {busy ? "…" : "Grant"}
        </button>
      </div>

      {error && <div className="banner error small">{error}</div>}

      {grants === null ? (
        <p className="muted small">Loading…</p>
      ) : grants.length === 0 ? (
        <p className="muted small">Not shared with anyone yet.</p>
      ) : (
        <ul className="grant-list">
          {grants.map((g) => (
            <li key={g.id} className="row small">
              <span style={{ flex: 1 }}>
                <strong>{g.grantee}</strong>{" "}
                <span className="muted">· since {formatDate(g.created_at)}</span>
              </span>
              <select
                value={g.role}
                onChange={(e) => void changeRole(g, e.target.value as Role)}
                aria-label={`role for ${g.grantee}`}
              >
                {ROLES.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
              <button className="link danger" onClick={() => void revoke(g)}>
                Revoke
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function describe(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 404) return "User sharing isn't available on this server yet.";
    return e.message;
  }
  return e instanceof Error ? e.message : "Unknown error";
}
