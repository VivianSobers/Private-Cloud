// Offline pinning: keep chosen files available without a connection.
//
// The file's bytes are stored in a dedicated Cache Storage bucket (pc-pinned) via
// the Cache API; the service worker serves that bucket when the network is gone
// (see public/sw.js). A small index in localStorage remembers *which* files are
// pinned and their names, so the Offline view can list them — Cache Storage keys
// on URLs alone and cannot, by itself, answer "what have I pinned".
//
// Only the original content URL is pinned (no ?variant), which is exactly the URL
// the Offline list and a plain open use, so the cache key always matches.

import { api } from "./api";

const PIN_CACHE = "pc-pinned";
const INDEX_KEY = "pc-pinned-index";

export interface PinnedFile {
  id: string;
  name: string;
  path: string;
  size?: number;
}

/** supportsPinning reports whether this browser can store offline files. */
export function supportsPinning(): boolean {
  return typeof caches !== "undefined";
}

function readIndex(): PinnedFile[] {
  try {
    const raw = localStorage.getItem(INDEX_KEY);
    return raw ? (JSON.parse(raw) as PinnedFile[]) : [];
  } catch {
    return [];
  }
}

function writeIndex(list: PinnedFile[]) {
  localStorage.setItem(INDEX_KEY, JSON.stringify(list));
}

/** listPinned returns the pinned files, newest first. */
export function listPinned(): PinnedFile[] {
  return readIndex();
}

/** isPinned reports whether a file id is pinned. */
export function isPinned(id: string): boolean {
  return readIndex().some((f) => f.id === id);
}

/** pinFile downloads and stores a file's bytes for offline use, and records it in
 *  the index. Throws if the fetch fails (e.g. offline, or not authorized). */
export async function pinFile(f: PinnedFile): Promise<void> {
  const cache = await caches.open(PIN_CACHE);
  // add() issues a same-origin request (carrying the session cookie) and stores
  // the response; a non-2xx status rejects, so a failure never records a phantom
  // pin.
  await cache.add(api.contentUrl(f.id));
  const list = readIndex().filter((x) => x.id !== f.id);
  list.unshift(f);
  writeIndex(list);
}

/** unpinFile removes a file's offline copy and its index entry. */
export async function unpinFile(id: string): Promise<void> {
  const cache = await caches.open(PIN_CACHE);
  await cache.delete(api.contentUrl(id));
  writeIndex(readIndex().filter((f) => f.id !== id));
}
