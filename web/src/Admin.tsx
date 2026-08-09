import { useCallback, useEffect, useState } from "react";

import { api, ApiError, formatBytes, formatDate, type AdminUser, type AuditEntry } from "./api";

// The admin console (Phase 7): manage user accounts and read the audit log. Only
// rendered for admins; every endpoint is 403 for non-admins server-side too, so
// this is convenience, not the security boundary. Degrades gracefully where the
// server has not implemented the admin surface yet.

type Tab = "users" | "audit";

export function Admin() {
  const [tab, setTab] = useState<Tab>("users");
  return (
    <section className="stack">
      <nav className="row small">
        <button className="link" aria-current={tab === "users"} onClick={() => setTab("users")}>
          Users
        </button>
        <button className="link" aria-current={tab === "audit"} onClick={() => setTab("audit")}>
          Audit log
        </button>
      </nav>
      {tab === "users" ? <Users /> : <Audit />}
    </section>
  );
}

function Users() {
  const [users, setUsers] = useState<AdminUser[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [newName, setNewName] = useState("");

  const load = useCallback(() => {
    api
      .adminUsers()
      .then((r) => setUsers(r.users))
      .catch((e) => setError(describe(e)));
  }, []);
  useEffect(load, [load]);

  const patch = useCallback(
    async (u: AdminUser, p: Parameters<typeof api.updateUser>[1]) => {
      try {
        await api.updateUser(u.id, p);
        load();
      } catch (e) {
        setError(describe(e));
      }
    },
    [load],
  );

  const create = useCallback(async () => {
    const username = newName.trim();
    if (!username) return;
    try {
      await api.createUser({ username });
      setNewName("");
      setError(null);
      load();
    } catch (e) {
      setError(describe(e));
    }
  }, [newName, load]);

  if (error && !users) return <Unavailable detail={error} />;

  return (
    <div className="stack">
      <div className="row">
        <input
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
          placeholder="new username"
          autoComplete="off"
          onKeyDown={(e) => e.key === "Enter" && void create()}
        />
        <button className="primary" onClick={() => void create()}>
          Create user
        </button>
      </div>
      {error && <div className="banner error small">{error}</div>}

      {!users ? (
        <p className="muted">Loading…</p>
      ) : (
        <div className="table-scroll">
          <table className="admin-table">
            <thead>
              <tr>
                <th>User</th>
                <th>Storage</th>
                <th>Admin</th>
                <th>Enabled</th>
                <th>Quota</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <UserRow key={u.id} user={u} onPatch={patch} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function UserRow({
  user,
  onPatch,
}: {
  user: AdminUser;
  onPatch: (u: AdminUser, p: Parameters<typeof api.updateUser>[1]) => void;
}) {
  const [quotaGiB, setQuotaGiB] = useState(String(Math.round(user.quota_bytes / 1024 ** 3)));
  const gib = 1024 ** 3;
  return (
    <tr className={user.disabled ? "row-disabled" : ""}>
      <td>
        <strong>{user.username}</strong>
        {user.display_name && <div className="muted small">{user.display_name}</div>}
      </td>
      <td className="small">
        {user.used_bytes != null ? formatBytes(user.used_bytes) : "—"}
        {user.quota_bytes > 0 && <span className="muted"> / {formatBytes(user.quota_bytes)}</span>}
      </td>
      <td>
        <input
          type="checkbox"
          checked={user.is_admin}
          onChange={(e) => onPatch(user, { is_admin: e.target.checked })}
          aria-label={`admin for ${user.username}`}
        />
      </td>
      <td>
        <input
          type="checkbox"
          checked={!user.disabled}
          onChange={(e) => onPatch(user, { disabled: !e.target.checked })}
          aria-label={`enabled for ${user.username}`}
        />
      </td>
      <td>
        <span className="row small">
          <input
            type="number"
            min="0"
            value={quotaGiB}
            onChange={(e) => setQuotaGiB(e.target.value)}
            style={{ width: "4.5rem" }}
            aria-label={`quota GiB for ${user.username}`}
          />
          GiB
          <button
            className="link"
            onClick={() => onPatch(user, { quota_bytes: Math.max(0, Number(quotaGiB)) * gib })}
          >
            Save
          </button>
        </span>
      </td>
    </tr>
  );
}

function Audit() {
  const [entries, setEntries] = useState<AuditEntry[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [actor, setActor] = useState("");
  const [action, setAction] = useState("");

  const load = useCallback(() => {
    api
      .adminAudit({ actor: actor || undefined, action: action || undefined, limit: 100 })
      .then((r) => setEntries(r.entries))
      .catch((e) => setError(describe(e)));
  }, [actor, action]);
  useEffect(load, [load]);

  if (error && !entries) return <Unavailable detail={error} />;

  return (
    <div className="stack">
      <div className="row small">
        <input value={actor} onChange={(e) => setActor(e.target.value)} placeholder="actor" />
        <input value={action} onChange={(e) => setAction(e.target.value)} placeholder="action" />
        <button onClick={load}>Filter</button>
      </div>
      {error && <div className="banner error small">{error}</div>}
      {!entries ? (
        <p className="muted">Loading…</p>
      ) : entries.length === 0 ? (
        <p className="muted">No matching events.</p>
      ) : (
        <div className="table-scroll">
          <table className="admin-table">
            <thead>
              <tr>
                <th>When</th>
                <th>Actor</th>
                <th>Action</th>
                <th>Target</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => (
                <tr key={e.id}>
                  <td className="small muted">{formatDate(e.at)}</td>
                  <td>{e.actor}</td>
                  <td>
                    <code>{e.action}</code>
                  </td>
                  <td className="small">
                    {e.target}
                    {e.request_id && <span className="muted"> · {e.request_id}</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function Unavailable({ detail }: { detail: string }) {
  return (
    <div className="stack">
      <p className="muted">The admin API isn't available on this server yet.</p>
      <p className="muted small">{detail}</p>
    </div>
  );
}

function describe(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 404) return "The admin endpoints are not deployed on this server yet.";
    if (e.status === 403) return "You do not have admin access.";
    return `${e.code}: ${e.message}`;
  }
  return e instanceof Error ? e.message : "Unknown error";
}
