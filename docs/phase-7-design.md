# Phase 7 — Multi-user & sharing (Vivian's front-of-API) design

**Status: 🟠 three of five client pieces shipped, and the server behind them is now
✅ complete.**

When this document was first written the server handlers did not exist, so every
panel degraded to a clear "not available on this server yet" state. That is no
longer the situation: [phase-7-server-design.md](phase-7-server-design.md) shipped
all four server slices, and these views light up unchanged — which was the entire
point of the seam, and is worth recording as a success rather than quietly
updating.

| Client piece | Status |
|---|---|
| Share with people (grant, change role, revoke) | ✅ |
| Shared with me (the roots others granted) | ✅ |
| Admin console: users, quotas, audit log | ✅ |
| Browsing **into** a granted folder (`?include_shared=true`) | ❌ |
| Per-user session management in the console | ❌ |

Marks: ✅ done · 🟠 partial · ❌ not built; the ledger is [status.md](status.md).

## ✅ What shipped (client)

**Share with people** ([web/src/PeopleShare.tsx](../web/src/PeopleShare.tsx)) —
inside the existing share dialog, alongside the public link: grant another
account `viewer` or `editor` on a node, change a role, or revoke it, against
`POST /nodes/{id}/grants`, `PATCH /grants/{id}`, `DELETE /grants/{id}`. Only the
**direct** grants on the node are shown and revocable; an inherited grant belongs
to the ancestor that carries it, so surfacing it here would imply it could be
revoked here, which it cannot.

**Shared with me** ([web/src/SharedWithMe.tsx](../web/src/SharedWithMe.tsx)) — a
top-level view reading `GET /shared`: the roots others granted me, each showing
the owner and my role from the node's `access` object, with a download for a
shared file. It deliberately does **not** widen the file browser — the contract's
rule is that shared content stays out of existing endpoints unless the client
opts in with `?include_shared=true`, so the default browser is unchanged and
shared content lives only in this dedicated view. (Opening a shared *folder*
inline waits on a shared-browser slice.)

**Admin console** ([web/src/Admin.tsx](../web/src/Admin.tsx)) — admin-only (the
nav entry is gated on `is_admin`, and every endpoint is `403` server-side too, so
this is convenience, not the boundary). A users table (`GET/POST/PATCH/DELETE
/admin/users`) to toggle admin/enabled, set a quota, and create accounts; and an
audit-log reader (`GET /admin/audit`) with actor/action filters.

## ✅ Why the split held again

All of this is `web/` only — typed client additions plus four components — so it
landed with zero contact with Guru's server work. The one shared file was the
contract, which Guru had already specified; the client codes against it exactly.
When the handlers shipped, **no UI change was needed** — the prediction this
section made is now a fact, and it is the strongest evidence in the repository that
the layer split was the right call.

## ❌ Not yet

- ❌ **The shared-folder browser** — navigating into a granted folder with
  `?include_shared=true`. The server has supported it since slice 2, and
  `include_shared` appears nowhere in `web/src`, so shared content is reachable
  only through the dedicated "Shared with me" view. A grantee cannot open a shared
  *folder* inline.
- ❌ **Per-user session management** in the admin console
  (`GET`/`DELETE /admin/users/{id}/sessions`). Both routes are served and are
  declared in `awaitingClient`; sign-out-everywhere is a `cloudctl` or SQL job
  today.
- 🟠 **The Phase 5 gallery wiring** is no longer open in the way this line
  originally meant: "add to album" and reordering both shipped, but reordering is
  move-up/move-down buttons rather than a pointer drag, and drag-select was never
  built. See [phase-5-design.md](phase-5-design.md) §7 slice 10.
