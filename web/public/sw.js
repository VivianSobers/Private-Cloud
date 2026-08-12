// Private Cloud service worker — makes the app installable and its shell available
// offline, without ever caching authenticated data.
//
// The rules are deliberately narrow:
//   - /api/*                never touched — always live, never cached.
//   - navigations           network-first, refreshing the cached shell, and
//                           falling back to that shell only when offline.
//   - /assets/* (hashed)    cache-first (immutable, content-addressed filenames),
//                           revalidated in the background.
//   - everything else       passes straight through.
//
// Bumping CACHE invalidates everything on the next activate.
const CACHE = "pc-shell-v2";
const PIN = "pc-pinned"; // offline-pinned file bytes, managed by the page (pin.ts)
const SHELL = "/index.html";

self.addEventListener("install", (event) => {
  // Take over as soon as the new worker is ready; the shell is populated by the
  // first online navigation rather than precached against stale asset hashes.
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      // Keep the current shell cache AND the pinned-files bucket; a shell version
      // bump must never evict a user's offline files.
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE && k !== PIN).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // Pinned file content: try the network first (fresh, and it revalidates auth),
  // and fall back to the offline pin bucket when the network is gone. Only the
  // bare content URL is handled — the exact key the page pins — so nothing else
  // under /api is ever cached.
  if (/^\/api\/v1\/nodes\/[^/]+\/content$/.test(url.pathname)) {
    event.respondWith(
      fetch(req).catch(() =>
        caches
          .open(PIN)
          .then((c) => c.match(req))
          .then((hit) => hit || Response.error()),
      ),
    );
    return;
  }

  // The rest of the API is authenticated and must always reach the network.
  if (url.pathname.startsWith("/api/")) return;

  // App shell: serve the network, keep the cached copy fresh, fall back offline.
  if (req.mode === "navigate") {
    event.respondWith(
      fetch(req)
        .then((res) => {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(SHELL, copy));
          return res;
        })
        .catch(() => caches.match(SHELL)),
    );
    return;
  }

  // Hashed build assets: cache-first, refresh in the background.
  if (url.pathname.startsWith("/assets/")) {
    event.respondWith(
      caches.match(req).then((hit) => {
        const network = fetch(req).then((res) => {
          if (res.ok) caches.open(CACHE).then((c) => c.put(req, res.clone()));
          return res;
        });
        return hit || network;
      }),
    );
  }
});
