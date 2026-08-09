# Phase 7 — Multi-user & sharing (Vivian's front-of-API) design

**Status: front-of-API in progress.** The web client for the collaboration and
admin surface, built against the Phase 7 contract Guru authored in
[api-contract.md](api-contract.md). The server handlers are not implemented yet,
so every panel here degrades to a clear "not available on this server yet" state
and lights up unchanged when the endpoints land at those exact shapes.

## What shipped (client)

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

## Why the split held again

All of this is `web/` only — typed client additions plus four components — so it
landed with zero contact with Guru's server work. The one shared file was the
contract, which Guru had already specified; the client codes against it exactly.
When the handlers ship, no UI change is needed.

## Not yet

- The shared-folder browser (navigating into a granted folder with
  `?include_shared=true`).
- Per-user session management in the admin console
  (`GET/DELETE /admin/users/{id}/sessions`).
- Drag-reorder and "add to album" wiring for the Phase 5 gallery remain open too.
