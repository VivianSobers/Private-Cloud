# Phase 7 — Multi-user & sharing (behind the API) design

**Status: ✅ complete — 4/4 slices behind the API, and now consumed.** Two things
remain: ❌ per-user API rate limiting (slice 5, deferred since Phase 4) and 🟠 the
last-admin guard, whose *refusal* path has no integration test for a reason recorded
in §4. In front of the API one consumer is still ❌ missing — the shared-folder
browser; see [phase-7-design.md](phase-7-design.md).

Marks: ✅ done · 🟠 partial · ❌ not built; the ledger is [status.md](status.md).

The server half of the phase whose client half is described in
[phase-7-design.md](phase-7-design.md). Those UIs — share-with-people,
shared-with-me, the admin console — were built against the contract months before
any of this existed and degraded to "not available on this server yet". They lit up
unchanged when it landed, which was the point of the seam, and the console has since
grown session revocation and a storage tab on top of the same surface.

**Exit criterion:** two accounts on one server can share a folder in either
direction, each seeing exactly what they were given and nothing else; an
administrator can provision, quota and disable accounts without an SSH session;
and every access decision is answerable after the fact from a log.

---

## 0. The hazard this phase carries

The API contract flags it: **every query in the system filtered
`owner_id = $me`**. Introducing "files I can see but do not own" widens what the
existing endpoints *mean* without changing their shape — the worst kind of
break, because nothing errors and no client fails to parse. A file browser
written in Phase 1 assumed everything it listed was its own.

So the widening is **opt-in per request**:

```
GET /nodes/{id}/children?include_shared=true
GET /search?include_shared=true
GET /tags?include_shared=true
```

Without the parameter, every one of those returns exactly what it returned
before, byte for byte. There is a test asserting that a grantee's own root
listing and default search are unaffected by a share existing. Anything other
than an explicit `true` reads as false, so a typo widens nothing.

**Two reads are deliberately *not* gated**: `GET /nodes/{id}` and
`GET /nodes/{id}/content`. The flag exists to stop a *listing* silently handing a
client rows it did not ask for. Both of these name a single node explicitly — the
caller either asked for that id or did not — so there is nothing to be surprised
by, and gating them would make `/shared` incoherent, handing out ids the client
is then refused permission to fetch. A viewer grant that cannot read the bytes is
not a share.

## 1. The model

A grant says: *this person may read (or write) this node and everything under
it*. Nothing else moves.

**Inheritance is derived, not stored.** A folder share has to cover files that do
not exist yet, so inheritance is a prefix test on the node's materialised path
rather than a row per descendant. Expanding grants into rows would have to be
maintained on every create, move, rename and restore — four places to get it
wrong, each of which silently grants or removes access. This is what the
denormalised `path` was put there for; the README has said so since Phase 1.

The prefix test uses **`starts_with`, not `LIKE`**, and a test is why. The
pattern has to be built from a column, so the Go-side `likePrefix` escaping
cannot reach it, and an unescaped column in a `LIKE` pattern turns every
metacharacter in a folder *name* into a wildcard: a grant on `100%_done` also
matched `1009Xdone`. That is a grant silently covering files nobody shared. The
trailing separator matters for the same class of reason — bare prefixes make a
grant on `/project` cover `/projectX`.

**One predicate, spliced everywhere.** `VisibleNodes` is a single SQL constant
used by children, search, semantic search and tags. The one thing that must never
vary between those is what "visible" means; three slightly different wordings is
how a file becomes readable through one endpoint and not another.

### Decisions worth stating

- **Owner is checked first** and never consults the grants table, so the
  overwhelmingly common case cannot be broken by a bug in the ACL layer.
- **Two grants give the union**, not whichever sorts first: editor beats viewer.
- **"No access" and "no such node" are the same answer.** Distinguishing them
  tells an unauthorised caller that an id is real. Granting to an unknown
  username returns that same 404, so probing cannot enumerate accounts.
- **Only the owner may grant.** An editor re-sharing would spread access
  transitively beyond what the owner can see or revoke — the failure mode that
  makes people stop trusting sharing entirely.
- **Either party may revoke.** The owner takes it back; the grantee declines. A
  grantee who cannot clear somebody else's folder out of their own "shared with
  me" has no way to tidy up, and declining costs them only access they could have
  ignored.
- **`owner` is a real role but not a grantable one** — a `CHECK` keeps it out of
  the table, because a row claiming ownership would be a second, contradictory
  source of truth about who owns a node.

## 2. What an editor may actually do

"Reads and writes" has to mean writes that land in the **owner's** tree and on
the **owner's** quota, or sharing a folder becomes a way to spend someone else's
storage.

So a write resolves the *node's* owner, not the caller: an upload into a shared
folder is owned by the folder's owner exactly as if they had created it
themselves. Charging the editor instead would either let one user spend another's
quota, or leave a file sitting in one tree while counting against a different
allowance — and then neither party could explain their own usage number.

Two edges follow from that and are tested:

- **An editor cannot move a shared file into their own tree.** Both ends of a
  move are resolved against the same owner. Otherwise it is a copy the owner
  never agreed to plus a silent transfer of bytes onto the editor's quota.
- **An editor's delete goes to the *owner's* trash**, so the owner can restore
  it. A soft delete an editor could make unrecoverable is a much sharper edge
  than "editor" implies.

## 3. Search, tags and the ACL

**Semantic search filters the NODE rows, never the vectors.** Embeddings are
content-addressed, so two users owning the same document share one vector row by
construction. Filtering vectors would either hide a document from someone
entitled to it or — far worse — let one user's query surface the existence of
another's document through a similarity score.

**Tag counts are per caller.** Two people can both have a `receipts` tag; a
global count tells each of them how many files the other has tagged. That is an
existence leak through a number.

## 4. Admin, and what it deliberately cannot do

`cloudctl` on the server stays the break-glass path and remains strictly more
powerful — it needs shell access, which already implies database and file access,
so it weakens nothing. These endpoints exist so routine account work does not
need an SSH session. Every route is admin-only **server-side**; the client's nav
gating is convenience, never the boundary, and a test walks all of them as a
non-admin expecting `403`.

**`DELETE /admin/users/{id}` disables and revokes. It does not delete.** Deleting
a user cascades their files away, and "remove this person's access" almost never
means "destroy everything they ever uploaded". Making that irreversible step one
console button away is how it happens by accident.

Disabling revokes every live session in the same call: an account whose browser
tab keeps working is not disabled.

**`quota_bytes` distinguishes absent from null**, because for a nullable column
those are opposite instructions — leave it alone versus clear it — and
unmarshalling into a `*int64` leaves the pointer nil for both. Absent in a
*response* means unlimited; sending `0` would be a quota of zero bytes, the
opposite of what a missing quota means.

Creating a user returns recovery codes **once**. A new account has no passkey and
no way to enrol one without first signing in, so it redeems a code and then
registers — reusing the recovery path rather than inventing an invite-token
concept.

### 🟠 The last-admin guard, and an honest note on its coverage

`UpdateUser` refuses to demote or disable the only enabled administrator. Locking
every admin out of their own server is not a state one request should be able to
reach; recovery is a shell and a SQL prompt.

**The refusal path is not covered by an integration test.** Its triggering
condition is a global property of the `users` table, and the suite shares one
database in which every fixture creates another admin — so an assertion would
either never fire, or require disabling accounts that sibling packages are
concurrently using, which is exactly the shared-state flakiness this suite
already suffers from. The succeeding path is tested. This is recorded rather than
papered over.

## 5. The audit log

Records **authorisation-relevant** events — grants, role changes, admin actions,
and writes into somebody else's tree — and not reads. A log that records
everything is one nobody reads, and on this hardware it would outgrow the files
it describes. An editor's write into a shared folder is logged; the same person's
upload into their own folder is not, or the log drowns in ordinary traffic.

Entries carry the `request_id`, so one ties back to the API access log without a
second correlation scheme.

`actor_id` is `ON DELETE SET NULL` with the username denormalised alongside it:
deleting a user must not erase the record of what they did, and an entry that can
no longer say who did the thing has lost the only fact that made it worth
keeping.

The write is **best effort and detached from the request context**. A grant that
succeeded and was not logged is a gap in the record; a grant refused because the
log was busy is a broken feature.

## 6. Slices

| # | Slice | Status |
|---|---|---|
| **1** | `grants` + `audit_log` schema (`00022`); `AccessFor`, inheritance, batch resolution | ✅ 16 tests |
| **2** | Grant endpoints, `/shared`, `?include_shared=` on children/search/tags | ✅ 11 tests |
| **3** | Admin users, sessions, audit endpoints | ✅ 8 tests |
| **4** | Editor writes: owner-charged quota, move and delete semantics | ✅ 7 tests |
| 5 | Per-user API rate limiting | ❌ still deferred — see [phase-4-hardening.md](phase-4-hardening.md) §5. Now more relevant than it was: a second user is no longer hypothetical, and Phase 8 gave one request a GPU box to spend |

**Consumed by a client?** Slices 1–4 are ✅ served and ✅ consumed — including
`GET`/`DELETE /admin/users/{id}/sessions`, which shipped with the admin console's
session view and was deleted from `awaitingClient` as a result.

One thing remains unconsumed, and it is a query parameter rather than a route, so no
test guards it: **`?include_shared=true`** on children, search and tags. Nothing in
`web/` sends it. The ACL work is therefore reachable through `/shared` and through
`POST /chat` (which does pass `include_shared`), but a grantee cannot browse *into* a
shared folder. The design's central compatibility decision — opt-in widening — is
implemented and, for listings, still unexercised.

## 7. Risks

- **Quota is charged to the owner, and an editor can fill it.** That is the
  correct owner for the bytes, but nothing yet lets an owner bound how much an
  editor may add. A malicious or careless editor can exhaust the owner's quota;
  the owner's remedy is revocation after the fact.
- **Grants survive a move.** A grant is on a node, and inheritance follows the
  path, so moving a shared folder moves the share with it — correct, but it means
  an owner can silently change what a grant covers by moving files into or out of
  a shared folder. Visible in `GET /grants`, but not announced.
- **Per-user rate limiting is still absent**, and this phase makes it matter
  more: semantic search over a shared corpus is an RPC to the sidecar that any
  authenticated user can now issue against content they do not own.
