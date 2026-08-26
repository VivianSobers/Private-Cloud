import { useCallback, useEffect, useState } from "react";

import {
  api,
  ApiError,
  formatBytes,
  formatDate,
  type AdminSession,
  type AdminStorage,
  type AdminUser,
  type AuditEntry,
  type BillingPlan,
  type MeteringRecord,
} from "./api";

// The admin console (Phase 7): manage user accounts and read the audit log. Only
// rendered for admins; every endpoint is 403 for non-admins server-side too, so
// this is convenience, not the security boundary. Degrades gracefully where the
// server has not implemented the admin surface yet.

type Tab = "users" | "storage" | "billing" | "audit";

export function Admin() {
  const [tab, setTab] = useState<Tab>("users");
  return (
    <section className="stack">
      <nav className="row small">
        <button className="link" aria-current={tab === "users"} onClick={() => setTab("users")}>
          Users
        </button>
        <button className="link" aria-current={tab === "storage"} onClick={() => setTab("storage")}>
          Storage
        </button>
        <button className="link" aria-current={tab === "billing"} onClick={() => setTab("billing")}>
          Billing
        </button>
        <button className="link" aria-current={tab === "audit"} onClick={() => setTab("audit")}>
          Audit log
        </button>
      </nav>
      {tab === "users" ? (
        <Users />
      ) : tab === "storage" ? (
        <Storage />
      ) : tab === "billing" ? (
        <Billing />
      ) : (
        <Audit />
      )}
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
  const [showSessions, setShowSessions] = useState(false);
  const gib = 1024 ** 3;
  // Empty, not "0", when there is no quota. A quota of zero bytes is a real and
  // very different thing from no quota at all, and the box has to be able to say
  // which one this account has.
  const [quotaGiB, setQuotaGiB] = useState(
    user.quota_bytes == null ? "" : String(Math.round(user.quota_bytes / gib)),
  );

  // An empty box clears the quota. The server distinguishes an absent field
  // ("leave it alone") from an explicit null ("make it unlimited"), so this is
  // the one control that can express the difference.
  const save = () => {
    const trimmed = quotaGiB.trim();
    if (trimmed === "") {
      onPatch(user, { quota_bytes: null });
      return;
    }
    const gigs = Number(trimmed);
    if (!Number.isFinite(gigs) || gigs < 0) return;
    onPatch(user, { quota_bytes: Math.round(gigs * gib) });
  };
  return (
    <>
    <tr className={user.disabled ? "row-disabled" : ""}>
      <td>
        <strong>{user.username}</strong>
        {user.display_name && <div className="muted small">{user.display_name}</div>}
        <div>
          <button
            className="link small"
            aria-expanded={showSessions}
            onClick={() => setShowSessions((v) => !v)}
          >
            {showSessions ? "Hide sessions" : "Sessions"}
          </button>
        </div>
      </td>
      <td className="small">
        {user.used_bytes != null ? formatBytes(user.used_bytes) : "—"}
        {user.quota_bytes != null ? (
          <span className="muted"> / {formatBytes(user.quota_bytes)}</span>
        ) : (
          <span className="muted"> / unlimited</span>
        )}
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
            placeholder="none"
            onChange={(e) => setQuotaGiB(e.target.value)}
            style={{ width: "4.5rem" }}
            aria-label={`quota GiB for ${user.username}`}
          />
          GiB
          <button className="link" onClick={save}>
            Save
          </button>
        </span>
      </td>
    </tr>
    {showSessions && (
      <tr className="session-detail">
        <td colSpan={5}>
          <UserSessions userId={user.id} username={user.username} />
        </td>
      </tr>
    )}
    </>
  );
}

/** UserSessions lists another account's sessions for an admin and lets them
 *  revoke one — the answer to "sign this user out everywhere" after a lost
 *  device or a compromised account, without resetting their credentials. */
function UserSessions({ userId, username }: { userId: string; username: string }) {
  const [sessions, setSessions] = useState<AdminSession[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api
      .adminUserSessions(userId)
      .then((r) => setSessions(r.sessions))
      .catch((e) => setError(describe(e)));
  }, [userId]);
  useEffect(load, [load]);

  const revoke = async (id: string) => {
    setBusy(true);
    try {
      await api.adminRevokeUserSession(userId, id);
      load();
    } catch (e) {
      setError(describe(e));
    } finally {
      setBusy(false);
    }
  };

  if (error) return <div className="banner error small">{error}</div>;
  if (!sessions) return <p className="muted small">Loading sessions…</p>;
  if (sessions.length === 0) return <p className="muted small">No active sessions.</p>;

  return (
    <table className="admin-table">
      <thead>
        <tr>
          <th>Session for {username}</th>
          <th>Last seen</th>
          <th />
        </tr>
      </thead>
      <tbody>
        {sessions.map((s) => (
          <tr key={s.id}>
            <td className="small">
              {s.user_agent || "unknown device"}
              {s.kind !== "web" && <span className="muted"> · {s.kind}</span>}
            </td>
            <td className="small muted">{formatDate(s.last_seen_at)}</td>
            <td>
              <button className="link danger" disabled={busy} onClick={() => void revoke(s.id)}>
                Revoke
              </button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
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

function Storage() {
  const [data, setData] = useState<AdminStorage | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .adminStorage()
      .then(setData)
      .catch((e) => setError(describe(e)));
  }, []);

  if (error && !data) return <Unavailable detail={error} />;
  if (!data) return <p className="muted">Loading…</p>;

  return (
    <div className="stack">
      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Accounted storage</h2>
        <p className="muted small" style={{ margin: 0 }}>
          What the database accounts for across every owner — not pool capacity.
          The application knows what it stored; the disks below know what they hold.
        </p>
        <div className="stat-row">
          <Stat label="Stored" value={formatBytes(data.accounted.stored_bytes)} />
          <Stat label="In trash" value={formatBytes(data.accounted.trash_bytes)} />
          <Stat label="Files" value={String(data.accounted.file_count)} />
        </div>
      </section>

      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Cold tier</h2>
        {!data.tiering.enabled ? (
          // Not a tier holding zero bytes — there is no tier. Rendering zeroes
          // here would imply the feature is on and merely empty, which is the
          // more misleading of the two answers.
          <p className="muted small" style={{ margin: 0 }}>
            {data.tiering.note ?? "No cold tier is configured; all content is on the local pool."}
          </p>
        ) : (
          <>
            <p className="muted small" style={{ margin: 0 }}>
              What the database says it moved to object storage — not what the
              bucket reports. The difference between the two is the discrepancy{" "}
              <code>cloudctl fsck</code> exists to find.
            </p>
            {data.tiering.cold_bytes == null ? (
              <p className="muted small" style={{ margin: 0 }}>
                {data.tiering.note ?? "The tier is configured; its accounting could not be read."}
              </p>
            ) : (
              <div className="stat-row">
                <Stat label="Cold" value={formatBytes(data.tiering.cold_bytes)} />
                <Stat label="Objects" value={String(data.tiering.cold_objects ?? 0)} />
                <Stat label="Blobs" value={String(data.tiering.cold_blobs ?? 0)} />
                <Stat label="Chunks" value={String(data.tiering.cold_chunks ?? 0)} />
              </div>
            )}
          </>
        )}
      </section>

      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Pools</h2>
        {data.pools.length === 0 ? (
          <p className="muted small" style={{ margin: 0 }}>
            No pool metrics.{" "}
            {data.collector.available
              ? `None reported under ${data.collector.path}.`
              : `The collector directory (${data.collector.path}) isn't readable here.`}
          </p>
        ) : (
          <div className="table-scroll">
            <table className="admin-table">
              <thead>
                <tr>
                  <th>Pool</th>
                  <th>State</th>
                  <th>Last scrub</th>
                  <th>Reported</th>
                </tr>
              </thead>
              <tbody>
                {data.pools.map((p) => (
                  <tr key={p.name}>
                    <td>
                      <strong>{p.name}</strong>
                    </td>
                    <td className={p.state === "ONLINE" ? "" : "danger"}>{p.state}</td>
                    <td className="small">{scrubText(p.last_scrub_age_seconds, p.last_scrub_clean)}</td>
                    <td className="small muted">
                      {p.collected_at ? formatDate(p.collected_at) : "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Offsite backup</h2>
        <div className="stat-row">
          <Stat
            label="Last success"
            value={data.backup.last_success_at ? formatDate(data.backup.last_success_at) : "never"}
          />
          <Stat
            label="Age"
            value={data.backup.age_seconds != null ? relAge(data.backup.age_seconds) : "—"}
          />
          {data.backup.last_failure_at && (
            <Stat label="Last failure" value={formatDate(data.backup.last_failure_at)} />
          )}
        </div>
      </section>

      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Jobs</h2>
        <div className="stat-row">
          {["queued", "running", "done", "failed"].map((k) => (
            <Stat key={k} label={k} value={String(data.jobs[k] ?? 0)} />
          ))}
        </div>
        <p className="muted small" style={{ margin: 0 }}>
          {data.tiering.enabled
            ? "A cold tier is configured."
            : (data.tiering.note ?? "No cold tier is configured; all content is on the local pool.")}
        </p>
      </section>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="stat">
      <div className="stat-value">{value}</div>
      <div className="muted small">{label}</div>
    </div>
  );
}

/** scrubText turns a pool's scrub age + clean flag into one honest phrase.
 *  Absent clean means never scrubbed — not the same as a scrub that found errors. */
function scrubText(ageSeconds?: number, clean?: boolean): string {
  if (ageSeconds == null && clean == null) return "never scrubbed";
  const when = ageSeconds != null ? relAge(ageSeconds) + " ago" : "unknown";
  if (clean === false) return `errors found · ${when}`;
  if (clean === true) return `clean · ${when}`;
  return when;
}

function relAge(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  if (s < 90) return `${s}s`;
  const m = Math.round(s / 60);
  if (m < 90) return `${m}m`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h}h`;
  return `${Math.round(h / 24)}d`;
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

// The billing tab (Phase 9 slice 5).
//
// Deliberately modest, because the feature is: a plan is a named quota, and
// assigning one writes through to the quota that has been enforced since Phase 7
// rather than becoming a second gate beside it. There is no payment provider
// here and the console does not pretend there is one — the price fields are
// metadata an external system reads through the webhook.
//
// The whole tab degrades to "not available on this server yet" on the 503 a
// server without the migrations answers with, exactly like every other optional
// surface in this console.
function Billing() {
  const [plans, setPlans] = useState<BillingPlan[] | null>(null);
  const [records, setRecords] = useState<MeteringRecord[] | null>(null);
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api
      .adminBillingPlans()
      .then((r) => setPlans(r.plans))
      .catch((e) => setError(describe(e)));
    // Metering and the user list are secondary: a failure in either leaves the
    // plans readable rather than blanking the tab. The records are what make a
    // closed period answerable, and nothing recovers last month's figure once
    // this month has overwritten the live number.
    api
      .adminMetering({ limit: 100 })
      .then((r) => setRecords(r.records))
      .catch(() => setRecords([]));
    api
      .adminUsers()
      .then((r) => setUsers(r.users))
      .catch(() => setUsers([]));
  }, []);

  useEffect(load, [load]);

  async function assign(userId: string, planId: string) {
    setBusy(true);
    try {
      await api.adminSetAccountPlan(userId, planId || null);
      load();
    } catch (e) {
      setError(describe(e));
    } finally {
      setBusy(false);
    }
  }

  if (error && !plans) return <Unavailable detail={error} />;
  if (!plans) return <p className="muted">Loading…</p>;

  return (
    <div className="stack">
      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Plans</h2>
        <p className="muted small" style={{ margin: 0 }}>
          A plan is a named quota. Putting an account on one writes its quota
          through to the account, so this drives the enforcement that already
          exists rather than sitting beside it.
        </p>
        {plans.length === 0 ? (
          <p className="muted small" style={{ margin: 0 }}>
            No plans defined. Every account keeps whatever quota it already has.
          </p>
        ) : (
          <div className="table-scroll">
            <table className="admin-table">
              <thead>
                <tr>
                  <th>Plan</th>
                  <th>Quota</th>
                  <th>Price</th>
                  <th>Accounts</th>
                </tr>
              </thead>
              <tbody>
                {plans.map((p) => (
                  <tr key={p.id}>
                    <td>
                      <strong>{p.name}</strong>
                      {p.description ? <div className="small muted">{p.description}</div> : null}
                    </td>
                    {/* null is unlimited and is a real value, not a missing one. */}
                    <td>{p.quota_bytes == null ? "Unlimited" : formatBytes(p.quota_bytes)}</td>
                    <td className="small">
                      {p.price_cents == null
                        ? "—"
                        : `${(p.price_cents / 100).toFixed(2)} ${p.currency || ""} / ${p.period}`}
                    </td>
                    <td className="small">{p.account_count ?? "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {plans.length > 0 && users.length > 0 ? (
        <section className="card stack">
          <h2 style={{ margin: 0, fontSize: "1rem" }}>Accounts</h2>
          <p className="muted small" style={{ margin: 0 }}>
            Taking an account off a plan leaves its quota where the plan put it.
            Clearing it would hand unlimited storage to somebody being removed
            from a paid tier, which is the opposite of what the action means.
          </p>
          <div className="table-scroll">
            <table className="admin-table">
              <thead>
                <tr>
                  <th>Account</th>
                  <th>Plan</th>
                </tr>
              </thead>
              <tbody>
                {users.map((u) => (
                  <tr key={u.id}>
                    <td>{u.username}</td>
                    <td>
                      <select
                        disabled={busy}
                        defaultValue=""
                        onChange={(e) => assign(u.id, e.target.value)}
                        aria-label={`Plan for ${u.username}`}
                      >
                        <option value="">No plan</option>
                        {plans.map((p) => (
                          <option key={p.id} value={p.id}>
                            {p.name}
                          </option>
                        ))}
                      </select>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      ) : null}

      <section className="card stack">
        <h2 style={{ margin: 0, fontSize: "1rem" }}>Metering</h2>
        <p className="muted small" style={{ margin: 0 }}>
          Usage snapshots, taken from the same numbers quotas are enforced
          against. Usage is a live measurement; nothing recovers a closed
          period's figure once the next one has overwritten it.
        </p>
        {!records || records.length === 0 ? (
          <p className="muted small" style={{ margin: 0 }}>
            No snapshots recorded yet.
          </p>
        ) : (
          <div className="table-scroll">
            <table className="admin-table">
              <thead>
                <tr>
                  <th>Account</th>
                  <th>Period</th>
                  <th>Total</th>
                  <th>Peak</th>
                  <th>Samples</th>
                </tr>
              </thead>
              <tbody>
                {records.map((rec) => (
                  <tr key={rec.id}>
                    <td>{rec.owner}</td>
                    <td className="small muted">
                      {formatDate(rec.period_start)} → {formatDate(rec.period_end)}
                    </td>
                    <td>{formatBytes(rec.total_bytes)}</td>
                    <td className="small">{formatBytes(rec.peak_total_bytes)}</td>
                    {/* Zero samples is "nobody was measuring", not "stored nothing". */}
                    <td className="small muted">{rec.samples}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
