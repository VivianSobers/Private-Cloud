# private-cloud web

React 19 + TypeScript, built with Vite. **Phase 1, slice 5.**

Served by Caddy as static files from the **same origin as the API**. That is
not a convenience: WebAuthn binds credentials to an origin, so a UI on a
different host could not use the passkeys this server issues.

## Development

```bash
npm install
npm run dev      # http://localhost:5173, proxying /api to localhost:8080
npm run build    # tsc --noEmit, then vite build -> dist/
```

`npm run build` typechecks before bundling, so a type error fails the build
rather than shipping a bundle that misuses the API.

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
src/App.tsx        shell, session state, view switching
src/SignIn.tsx     bootstrap, sign-in, recovery redemption
src/Browser.tsx    the file browser
src/Photos.tsx     photo timeline + albums (Phase 5 media surface)
src/Trash.tsx      restore and purge
src/Settings.tsx   passkeys, sessions, recovery codes, app passwords, audit
src/styles.css     hand-rolled, no framework
```

## Decisions worth knowing

**No CSS framework.** The whole stylesheet is smaller than the manifest of one,
and this is a single-purpose app whose layout will never need a grid system.
Both colour schemes are declared, because a file browser is something people
open at night.

**No router.** Two views and a modal do not need one, and the SPA fallback in
Caddy means deep links would need server cooperation anyway.

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
