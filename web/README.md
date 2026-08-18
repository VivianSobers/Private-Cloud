# private-cloud web

React 19 + TypeScript, built with Vite.

**Status: 🟠 wired to every shipped endpoint but three.** Shipped ✅: the file
browser, trash, versions, share links and share management, tags and tag browsing,
search (keyword and semantic), the photo timeline / lightbox / albums, Ask with
generated answers and mandatory citations, the People face browser, face correction
in the lightbox, find-similar, offline file pinning, share-with-people,
shared-with-me, a storage audit that runs `fsck`, device management, the admin
console (users · quotas · storage · audit log · per-user sessions) and an installable
PWA shell.

Still ❌ open, with the server side of each already live:

| Missing | Endpoint it would call | Note |
|---|---|---|
| Browsing into a granted folder | `?include_shared=true` on children/search/tags | shared content is reachable through the Shared view and through `/chat`, but a shared *folder* cannot be opened inline |
| Map view over photo GPS | the `gps` field, already typed in `api.ts` | needs a map library, and an offline-capable PWA is in tension with a tile provider that phones home |
| Web Push subscription | `POST /devices/{id}/push` | blocked behind the API, not here: the server publishes no VAPID public key, without which `PushManager.subscribe` cannot be called |
| PWA share target | — | |

🟠 Album reordering ships as move-up/move-down buttons in a "Manage" mode, not a
pointer drag; drag-select is not built. The endpoint contract (replace the whole
order in one call) is already satisfied by the buttons.

The ledger for all of this is [../docs/status.md](../docs/status.md); the
mechanically-enforced version is `awaitingClient` in
[../server/internal/httpapi/contract_test.go](../server/internal/httpapi/contract_test.go).

Served by Caddy as static files from the **same origin as the API**. That is
not a convenience: WebAuthn binds credentials to an origin, so a UI on a
different host could not use the passkeys this server issues.

## Development

```bash
npm install
npm run dev      # http://localhost:5173, proxying /api to localhost:8080
npm run build    # tsc --noEmit, then vite build -> dist/
npm test         # vitest run — the API/error layer and the offline-pin invariants
```

`npm run build` typechecks before bundling, so a type error fails the build
rather than shipping a bundle that misuses the API.

✅ **There is a test suite now** — 20 cases across [src/api.test.ts](src/api.test.ts)
and [src/pin.test.ts](src/pin.test.ts), covering the error-shape parsing every view
depends on and the pin index/cache invariants that decide whether an offline file is
still there. It closes the "the client has tests, the web app has none" gap; it is
not broad coverage of the components.

No Node installed? The whole thing builds in a container. Keep `node_modules`
in a named volume — installing thousands of small files into a Windows bind
mount is pathologically slow:

```bash
docker run --rm -v "$PWD:/app" -v pc-node-modules:/app/node_modules \
  -w /app node:22-alpine sh -c "npm install && npm run build"
```

In production the compose `web` service builds the bundle into a volume and
exits. There is no Node process running anywhere — shipping a runtime just to
serve pre-built assets would add attack surface for no benefit.

## Layout

```
src/api.ts         typed API client; one place that handles the error shape
src/webauthn.ts    ceremony plumbing — base64url <-> ArrayBuffer, error text
src/upload.ts      upload strategy: plain POST vs tus
src/main.tsx       entry point; registers the service worker in production only
src/App.tsx        shell, session state, view switching
src/SignIn.tsx     bootstrap, sign-in, recovery redemption
src/RecoveryCodes.tsx  show-once codes, gated behind "I have saved these"
src/Browser.tsx    the file browser
src/Versions.tsx   version history modal (Phase 2)
src/Trash.tsx      restore and purge
src/ShareDialog.tsx  create a public link; hosts the people panel too
src/Shares.tsx     manage existing links (Phase 2)
src/SharePage.tsx  the public /s/{token} view, on the share plane
src/Tags.tsx       per-node tag editing (Phase 4)
src/TagBrowser.tsx browse by tag, with counts (Phase 4)
src/Photos.tsx     photo timeline + albums (Phase 5 media surface)
src/Ask.tsx        ask-your-library: /chat answer + citations (Phase 8)
src/People.tsx     face clusters — view + name (Phase 8)
src/Offline.tsx    files pinned for offline access (Phase 6)
src/pin.ts         offline-pin store (Cache API + index)
src/PeopleShare.tsx  grant/revoke user access on a node (Phase 7)
src/SharedWithMe.tsx roots others shared with me (Phase 7)
src/Admin.tsx      admin console: users + audit log (Phase 7, admin-only)
src/Settings.tsx   passkeys, sessions, recovery codes, app passwords, fsck audit
src/styles.css     hand-rolled, no framework
```

Eight top-level views (Files · Photos · People · Ask · Shared · Offline · Admin ·
Settings), the Admin one gated on `is_admin` — client-side for convenience only;
every admin route is `403` server-side, which is the actual boundary.

## Decisions worth knowing

**No CSS framework.** The whole stylesheet is smaller than the manifest of one,
and this is a single-purpose app whose layout will never need a grid system.
Both colour schemes are declared, because a file browser is something people
open at night.

**No router.** Eight views switched by state, and the SPA fallback in Caddy means
deep links would need server cooperation anyway. This is the decision most likely to
need revisiting: it was comfortable at two views, and it is the reason there are no
shareable in-app URLs — a photo, an album, a person or a search cannot be linked
to.

**Uploads switch strategy at 8 MiB.** Below that, a plain `POST` — one request,
no session row, no staging file. Above it, tus, because a large transfer over a
phone connection will fail eventually and starting over from zero is the
difference between a usable file server and a toy.

**Small uploads use `XMLHttpRequest`, not `fetch`.** `fetch` still cannot report
upload progress in any shipping browser, and an upload with no progress bar is
indistinguishable from a hung one.

**Downloads are plain links.** Letting the browser handle them gives a real
progress indicator, resumable downloads, and the ability to stream a video
straight into a player — none of which a blob held in memory does.

**Recovery codes gate their own dismissal** behind an explicit "I have saved
these" checkbox. They are stored only as hashes and cannot be shown again, so a
stray click on a close button is not something the user can undo.

**Errors surface `request_id`.** Every API error carries one, and showing it is
what makes "it broke" answerable from the server logs without asking the user
to reproduce it.

**Installable PWA, with a deliberately narrow service worker.** A manifest and
icon (`public/manifest.webmanifest`, `public/icon.svg`) make the app installable
to a home screen or desktop; `public/sw.js` caches only the app *shell*, never
data. Its rules are strict: `/api/*` is never touched (authenticated, always
live); navigations are network-first with the cached shell as the offline
fallback; and hashed `/assets/*` are cache-first because their filenames are
content-addressed. The worker registers only in production and never on the
public `/s/` share plane. This is the foundation for the mobile client — the app
installs and its shell opens offline, with no native toolchain.
