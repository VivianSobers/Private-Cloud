// The receiving half of the PWA share target.
//
// When someone picks this app from their phone's share sheet, the browser POSTs
// the shared files to /share-target. That POST never reaches the server: the
// service worker intercepts it, puts the files in a Cache Storage bucket, and
// redirects to /?share=inbox. This module is what the page then reads.
//
// Cache Storage rather than IndexedDB or a message to the page, because neither
// of those is reliably available at the moment the POST arrives: the app may not
// be running at all, and a share must survive the round trip through a cold
// start. A cache entry is a Request/Response pair that already holds bytes and a
// content type, which is exactly what a shared file is.
//
// Nothing here uploads. The inbox is read, shown, and cleared by the user's
// decision — a share sheet that silently wrote files into somebody's cloud with
// no destination chosen would be the wrong default in the one direction that
// cannot be undone from the phone.

/** The bucket the service worker writes into. Keep in step with public/sw.js. */
export const SHARE_CACHE = "pc-share-inbox";

/** The query the service worker redirects to, so the app knows to look. */
export const SHARE_PARAM = "share";

export interface SharedItem {
  /** The cache key, which is also how the item is deleted once it is dealt with. */
  key: string;
  name: string;
  type: string;
  size: number;
  blob: Blob;
}

export function supportsSharing(): boolean {
  return typeof caches !== "undefined";
}

/** shareArrived reports whether this page load came from a share. */
export function shareArrived(search: string): boolean {
  return new URLSearchParams(search).get(SHARE_PARAM) === "inbox";
}

/** readInbox returns everything the share target has stashed and not yet dealt
 *  with. Empty rather than throwing when the bucket does not exist: arriving at
 *  ?share=inbox with nothing in it is an ordinary consequence of a reload. */
export async function readInbox(): Promise<SharedItem[]> {
  if (!supportsSharing()) return [];

  const cache = await caches.open(SHARE_CACHE);
  const keys = await cache.keys();
  const items: SharedItem[] = [];

  for (const request of keys) {
    const res = await cache.match(request);
    if (!res) continue;
    const blob = await res.blob();
    items.push({
      key: request.url,
      // The worker records the original filename in a header, because a cache
      // key is a URL and a URL cannot carry a name with a slash or a space in it
      // intact. Falling back to the last path segment keeps a nameless share
      // usable rather than dropping it.
      name: decodeName(res.headers.get("X-Share-Filename")) || lastSegment(request.url),
      type: res.headers.get("Content-Type") || "application/octet-stream",
      size: blob.size,
      blob,
    });
  }
  return items;
}

/** discard removes one item from the inbox — after it has been uploaded, or
 *  because the user does not want it. Both are the same operation: the inbox is
 *  a hand-off, not a record. */
export async function discard(key: string): Promise<void> {
  if (!supportsSharing()) return;
  const cache = await caches.open(SHARE_CACHE);
  await cache.delete(key);
}

/** clearInbox empties the bucket. Called when the user is done with the share,
 *  so a later visit does not re-offer files they already dealt with. */
export async function clearInbox(): Promise<void> {
  if (!supportsSharing()) return;
  await caches.delete(SHARE_CACHE);
}

/** The worker percent-encodes the filename, because an HTTP header cannot carry
 *  the characters a phone allows in one. A name that will not decode is treated
 *  as absent rather than shown raw. */
function decodeName(raw: string | null): string {
  if (!raw) return "";
  try {
    return decodeURIComponent(raw);
  } catch {
    return "";
  }
}

function lastSegment(url: string): string {
  try {
    const path = new URL(url).pathname;
    return decodeURIComponent(path.slice(path.lastIndexOf("/") + 1)) || "shared-file";
  } catch {
    return "shared-file";
  }
}
