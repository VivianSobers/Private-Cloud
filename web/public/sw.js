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
const SHARE = "pc-share-inbox"; // files handed over by the OS share sheet (share.ts)
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
      // Keep the current shell cache, the pinned-files bucket AND the share
      // inbox; a shell version bump must never evict a user's offline files, nor
      // a share that arrived seconds before an update landed.
      .then((keys) =>
        Promise.all(
          keys.filter((k) => k !== CACHE && k !== PIN && k !== SHARE).map((k) => caches.delete(k)),
        ),
      )
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;

  // The share target. The OS share sheet POSTs here; this POST is never meant to
  // reach the server, and would not know where to put the files if it did — the
  // destination is a decision the user has not made yet. The files are parked in
  // a cache and the page is opened to deal with them.
  //
  // The redirect is what makes the app open at all: a share target that answers
  // with a body leaves the user staring at a response instead of at their files.
  if (req.method === "POST" && new URL(req.url).pathname === "/share-target") {
    event.respondWith(receiveShare(req));
    return;
  }

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

// receiveShare parks the shared files and sends the user into the app.
//
// Each file becomes a cache entry under a synthetic URL, because Cache Storage
// keys on URLs and two shares of "photo.jpg" must not overwrite each other. The
// real filename travels in a header instead: it can contain spaces, slashes and
// anything else a phone allows, none of which survives being part of a key.
//
// It never fails the navigation. A share that cannot be stored still opens the
// app — with an empty inbox and a page that says so — rather than showing the OS
// share sheet an error the user cannot act on.
async function receiveShare(req) {
  try {
    const form = await req.formData();
    const files = form.getAll("files").filter((f) => f && typeof f !== "string");
    const cache = await caches.open(SHARE);
    const stamp = Date.now();

    for (const [i, file] of files.entries()) {
      const key = `/__share/${stamp}-${i}`;
      await cache.put(
        new Request(key),
        new Response(file, {
          headers: {
            "Content-Type": file.type || "application/octet-stream",
            "X-Share-Filename": encodeURIComponent(file.name || `shared-${i}`),
          },
        }),
      );
    }
  } catch {
    // Deliberately swallowed: see above. The user lands in the app either way.
  }
  // Absolute, resolved against this worker's own origin: a relative URL here is
  // legal but has been the source of enough cross-browser surprises that being
  // explicit costs nothing.
  return Response.redirect(new URL("/?share=inbox", self.location.origin).toString(), 303);
}

// --- web push (Phase 6) ------------------------------------------------------
//
// The payload is deliberately thin: a type and a change cursor, no filename and
// no preview. The client already knows how to turn a cursor into detail — it
// calls GET /changes — and a push travels through a browser vendor's
// infrastructure. The body is encrypted end to end so they cannot read it, but a
// system whose premise is that your files stay on your hardware should not put
// their names in a message routed through Google or Mozilla in the first place.
//
// So the notification says that something changed, not what.
self.addEventListener("push", (event) => {
  let seq = 0;
  try {
    const data = event.data ? event.data.json() : {};
    if (typeof data.seq === "number") seq = data.seq;
  } catch {
    // A payload we cannot parse still means "go and look", which is the whole
    // content of the message. Falling through to the generic notification is
    // strictly better than showing nothing.
  }

  // userVisibleOnly was promised at subscribe time, and browsers enforce it: a
  // push that shows no notification eventually costs the subscription. So this
  // always shows one.
  event.waitUntil(
    self.registration.showNotification("Private Cloud", {
      body: "Your library has new changes.",
      icon: "/icon-192.png",
      badge: "/icon-192.png",
      // Collapses repeats: several uploads in a row replace one notification
      // rather than stacking a column of identical ones.
      tag: "pc-changes",
      renotify: false,
      data: { seq },
    }),
  );
});

// Focus an existing tab rather than opening another one. Someone who already has
// the app open and taps a notification means "show me", not "give me a second
// copy of the app".
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  event.waitUntil(
    (async () => {
      const clientList = await self.clients.matchAll({
        type: "window",
        includeUncontrolled: true,
      });
      for (const client of clientList) {
        if (new URL(client.url).origin === self.location.origin) {
          await client.focus();
          return;
        }
      }
      await self.clients.openWindow("/");
    })(),
  );
});
