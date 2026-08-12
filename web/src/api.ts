// Typed client for the private-cloud API.
//
// Every response goes through one place so the error shape is handled once.
// The API returns {error:{code,message,request_id}} for every failure, and
// surfacing request_id to the user is what makes "it broke" answerable from
// the server logs without asking them to reproduce it.

export interface ApiErrorBody {
  error: { code: string; message: string; request_id?: string };
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly requestId?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export interface Node {
  id: string;
  kind: "folder" | "file";
  name: string;
  path: string;
  parent_id?: string;
  size?: number;
  mime?: string;
  /** Whole-file hash; the key names the algorithm. sha256 for whole-file
   *  blobs, blake3 for chunked (content-addressed) files. */
  sha256?: string;
  blake3?: string;
  trashed_at?: string;
  created_at: string;
  updated_at: string;
  /** Present on a single-node GET: the file's tags, auto and user. */
  tags?: NodeTag[];
  /** Media metadata, present once the `media` job has analysed the file's bytes. */
  media?: Media;
  /** Present only on a node the caller does not own (shared with them). Absent
   *  means the node is the caller's own. */
  access?: Access;
}

/** A tag on a file, and how it got there. */
export interface NodeTag {
  name: string;
  source: "auto" | "user";
}

/** Media metadata on a file. Every field is optional — a PNG has no `taken_at`,
 *  a video has no `gps` — so a gallery reads defensively. */
export interface Media {
  width?: number;
  height?: number;
  orientation?: number;
  /** When the shutter fired — what a timeline sorts by, NOT created_at. The
   *  client falls back to the node's updated_at itself when this is absent. */
  taken_at?: string;
  camera?: string;
  gps?: { lat: number; lon: number };
  duration_ms?: number;
  /** Which derived sizes exist right now, e.g. ["thumb","preview"], so a tile
   *  knows whether to ask for a thumbnail or fall back rather than 404 per tile. */
  variants?: string[];
}

/** How a node is shared with a user. `owner` is the file's actual owner and
 *  cannot be granted away. */
export type Role = "viewer" | "editor" | "owner";

/** A grant gives a user access to one node (and, for a folder, everything under
 *  it). Grants inherit down a folder; inherited_from names the ancestor a grant
 *  came from, so a UI can explain *why* someone has access. */
export interface Grant {
  id: string;
  node_id: string;
  path: string;
  owner: string;
  grantee: string;
  role: Role;
  inherited_from?: string;
  created_at: string;
}

/** Present on a node the caller does NOT own, once shared content is included.
 *  Its absence means "mine". */
export interface Access {
  role: Role;
  owner: string;
  shared: boolean;
}

/** A user account, as seen by an admin. */
export interface AdminUser {
  id: string;
  username: string;
  display_name: string;
  is_admin: boolean;
  disabled: boolean;
  /** Absent means unlimited — the server omits the key entirely rather than
   *  sending 0, because 0 would be a quota of zero bytes. Optional here for the
   *  same reason: typing it as always-present made `quota_bytes / 1024**3`
   *  render the string "NaN" in the admin console for every user without one,
   *  and TypeScript could not see it because the type said the field was there. */
  quota_bytes?: number;
  used_bytes?: number;
  created_at: string;
}

/** One authorisation-relevant event in the append-only audit log. */
export interface AuditEntry {
  id: string;
  at: string;
  actor: string;
  action: string;
  target: string;
  request_id?: string;
  detail?: Record<string, unknown>;
}

/** A user-ordered collection of nodes. NOT a folder: a node can be in many
 *  albums, and being in one does not move or copy it. */
export interface Album {
  id: string;
  name: string;
  description: string;
  item_count: number;
  cover_node_id?: string;
  created_at: string;
  updated_at: string;
}

/** A search result: a node, plus why the query matched it. */
export interface SearchHit extends Node {
  /** The query hit an ancestor folder rather than the file's own name. */
  matched_path?: boolean;
  /** The query hit the file's extracted text (an OCR'd receipt, say). */
  matched_content?: boolean;
  /** This is a semantic (meaning) match; score carries the similarity. */
  semantic?: boolean;
  score?: number;
}

/** One entry in a file's history. The id addresses immutable content, so it is
 *  exposed (unlike a node's storage detail): restore and download name it. */
export interface Version {
  id: string;
  size: number;
  mime: string;
  created_at: string;
  created_by?: string;
  is_head: boolean;
  sha256?: string;
  blake3?: string;
}

/** A share as its owner manages it. The token is absent — it is never stored,
 *  so it can only be shown once, at creation. */
export interface ShareInfo {
  id: string;
  file_name: string;
  path: string;
  created_at: string;
  has_password: boolean;
  download_count: number;
  revoked: boolean;
  expired: boolean;
  file_trashed: boolean;
  active: boolean;
  expires_at?: string;
  max_downloads?: number;
}

/** The one-time result of creating a share, including the token to copy now. */
export interface CreatedShare {
  id: string;
  token: string;
  path: string;
  has_password: boolean;
  expires_at?: string;
  max_downloads?: number;
}

export interface ShareEntry {
  name: string;
  kind: "file" | "folder";
  size?: number;
}

/** The public, leak-free view of a share. When locked, only has_password is set. */
export interface ShareView {
  has_password: boolean;
  unlocked: boolean;
  name?: string;
  kind?: "file" | "folder";
  size?: number;
  mime?: string;
  path?: string;
  entries?: ShareEntry[];
}

export interface User {
  id: string;
  username: string;
  display_name: string;
  is_admin: boolean;
  created_at: string;
}

export interface Me {
  user: User;
  session_kind: "web" | "device" | "recovery";
  remaining_recovery_codes: number;
}

export interface Usage {
  live_bytes: number;
  trash_bytes: number;
  total_bytes: number;
  file_count: number;
  quota_bytes?: number;
  available_bytes?: number;
}

export interface Credential {
  id: string;
  name: string;
  created_at: string;
  last_used_at?: string;
}

export interface AppPassword {
  id: string;
  name: string;
  created_at: string;
  last_used_at?: string;
  expires_at?: string;
}

export interface Session {
  id: string;
  kind: string;
  user_agent: string;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
  current: boolean;
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...init,
    // The session is an HttpOnly cookie; without this it is simply not sent.
    credentials: "same-origin",
    headers: {
      ...(init.body && !(init.body instanceof FormData) && !(init.body instanceof Blob)
        ? { "Content-Type": "application/json" }
        : {}),
      ...init.headers,
    },
  });

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  let body: unknown = undefined;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      // A non-JSON body means something upstream of the API answered — Caddy
      // returning a 502, most likely. Report it as-is rather than pretending
      // to have parsed an error shape that isn't there.
      //
      // Thrown for a 2xx as well, not only for an error. Every endpoint here
      // returns JSON, so unparseable content with a success status is a broken
      // response however encouraging the status line looks — and returning
      // undefined for it, as this used to, pushes the failure into the caller
      // as "cannot read property of undefined" somewhere unrelated.
      throw new ApiError(res.status, "unexpected_response", text.slice(0, 200));
    }
  }

  if (!res.ok) {
    const e = (body as ApiErrorBody | undefined)?.error;
    throw new ApiError(
      res.status,
      e?.code ?? "unknown",
      e?.message ?? `request failed (${res.status})`,
      e?.request_id,
    );
  }
  return body as T;
}

// --- auth -------------------------------------------------------------------

export const api = {
  authStatus: () =>
    request<{ bootstrap_required: boolean; user_count: number; oidc_enabled?: boolean }>(
      "/api/v1/auth/status",
    ),

  me: () => request<Me>("/api/v1/auth/me"),

  logout: () => request<{ status: string }>("/api/v1/auth/logout", { method: "POST" }),

  redeemRecovery: (username: string, code: string) =>
    request<Me & { next_step: string }>("/api/v1/auth/recovery/redeem", {
      method: "POST",
      body: JSON.stringify({ username, code }),
    }),

  regenerateRecovery: () =>
    request<{ recovery_codes: string[]; notice: string }>("/api/v1/auth/recovery/regenerate", {
      method: "POST",
    }),

  credentials: () => request<{ credentials: Credential[] }>("/api/v1/auth/credentials"),

  deleteCredential: (id: string) =>
    request<{ status: string }>(`/api/v1/auth/credentials/${id}`, { method: "DELETE" }),

  sessions: () => request<{ sessions: Session[] }>("/api/v1/auth/sessions"),

  appPasswords: () => request<{ app_passwords: AppPassword[] }>("/api/v1/auth/app-passwords"),

  createAppPassword: (name: string, ttlHours = 0) =>
    request<{ app_password: AppPassword; password: string; notice: string }>(
      "/api/v1/auth/app-passwords",
      { method: "POST", body: JSON.stringify({ name, ttl_hours: ttlHours }) },
    ),

  revokeAppPassword: (id: string) =>
    request<{ status: string }>(`/api/v1/auth/app-passwords/${id}`, { method: "DELETE" }),

  revokeSession: (id: string) =>
    request<{ status: string }>(`/api/v1/auth/sessions/${id}`, { method: "DELETE" }),

  // --- files ----------------------------------------------------------------

  root: () => request<{ node: Node }>("/api/v1/nodes/root"),

  node: (id: string) => request<{ node: Node }>(`/api/v1/nodes/${id}`),

  children: (id: string) =>
    request<{ parent: Node; children: Node[] }>(`/api/v1/nodes/${id}/children`),

  resolve: (path: string) =>
    request<{ node: Node }>(`/api/v1/nodes/resolve?path=${encodeURIComponent(path)}`),

  createFolder: (parentId: string, name: string) =>
    request<{ node: Node }>("/api/v1/folders", {
      method: "POST",
      body: JSON.stringify({ parent_id: parentId, name }),
    }),

  patchNode: (id: string, changes: { name?: string; parent_id?: string }) =>
    request<{ node: Node }>(`/api/v1/nodes/${id}`, {
      method: "PATCH",
      body: JSON.stringify(changes),
    }),

  trashNode: (id: string) =>
    request<{ status: string; nodes_affected: number }>(`/api/v1/nodes/${id}`, { method: "DELETE" }),

  trash: () => request<{ items: Node[] }>("/api/v1/trash"),

  restore: (id: string) => request<{ node: Node }>(`/api/v1/trash/${id}/restore`, { method: "POST" }),

  purge: (id: string) => request<{ status: string }>(`/api/v1/trash/${id}`, { method: "DELETE" }),

  emptyTrash: () => request<{ status: string; items_purged: number }>("/api/v1/trash", { method: "DELETE" }),

  usage: () => request<Usage>("/api/v1/usage"),

  search: (
    q: string,
    opts: { under?: string; kind?: string; limit?: number; offset?: number; semantic?: boolean } = {},
  ) => {
    const params = new URLSearchParams({ q });
    if (opts.under && opts.under !== "/") params.set("under", opts.under);
    if (opts.kind) params.set("kind", opts.kind);
    if (opts.limit) params.set("limit", String(opts.limit));
    // Without an offset the API's has_more is decoration: it says another page
    // exists and there is no way to ask for it.
    if (opts.offset) params.set("offset", String(opts.offset));
    // Semantic mode ranks by meaning via the embedding sidecar; a 503 here means
    // it is not enabled, which the caller can fall back from to lexical search.
    if (opts.semantic) params.set("semantic", "true");
    return request<{ query: string; results: SearchHit[]; count: number; has_more: boolean }>(
      `/api/v1/search?${params}`,
    );
  },

  // --- tags -------------------------------------------------------------------

  nodeTags: (id: string) => request<{ tags: NodeTag[] }>(`/api/v1/nodes/${id}/tags`),

  addTag: (id: string, tag: string) =>
    request<{ tags: NodeTag[] }>(`/api/v1/nodes/${id}/tags`, {
      method: "POST",
      body: JSON.stringify({ tag }),
    }),

  removeTag: (id: string, tag: string) =>
    request<{ status: string }>(`/api/v1/nodes/${id}/tags/${encodeURIComponent(tag)}`, {
      method: "DELETE",
    }),

  listTags: () => request<{ tags: { tag: string; count: number }[] }>("/api/v1/tags"),

  tagNodes: (tag: string, opts: { limit?: number; offset?: number } = {}) => {
    const params = new URLSearchParams();
    if (opts.limit) params.set("limit", String(opts.limit));
    if (opts.offset) params.set("offset", String(opts.offset));
    const qs = params.toString();
    return request<{ tag: string; nodes: Node[]; count: number; has_more: boolean }>(
      `/api/v1/tags/${encodeURIComponent(tag)}${qs ? `?${qs}` : ""}`,
    );
  },

  fsck: (repair: boolean) =>
    request<Record<string, unknown>>(`/api/v1/admin/fsck?repair=${repair}`, { method: "POST" }),

  // downloadUrl is a plain link rather than a fetch: letting the browser handle
  // it gives a real progress indicator, resumable downloads and the ability to
  // stream a video straight into <video>, none of which a blob in memory does.
  downloadUrl: (id: string, forceDownload = false) =>
    `/api/v1/nodes/${id}/content${forceDownload ? "?download=1" : ""}`,

  // --- media & albums (Phase 5) ---------------------------------------------

  // A plain <img src>: a variant of a file's content. The browser gets ETags,
  // range requests and its own cache for free, exactly like downloadUrl. `thumb`
  // may 404 until the media job has produced it — a tile handles that itself.
  contentUrl: (id: string, variant?: "thumb" | "preview" | "original") =>
    `/api/v1/nodes/${id}/content${variant ? `?variant=${variant}` : ""}`,

  // The gallery's primary read: media files sorted by taken_at, paged by date.
  timeline: (opts: { from?: string; to?: string; limit?: number; offset?: number } = {}) => {
    const p = new URLSearchParams();
    if (opts.from) p.set("from", opts.from);
    if (opts.to) p.set("to", opts.to);
    if (opts.limit != null) p.set("limit", String(opts.limit));
    if (opts.offset != null) p.set("offset", String(opts.offset));
    const q = p.toString();
    return request<{ items: Node[]; has_more: boolean }>(`/api/v1/media/timeline${q ? `?${q}` : ""}`);
  },

  albums: () => request<{ albums: Album[] }>("/api/v1/albums"),

  album: (id: string, opts: { limit?: number; offset?: number } = {}) => {
    const p = new URLSearchParams();
    if (opts.limit != null) p.set("limit", String(opts.limit));
    if (opts.offset != null) p.set("offset", String(opts.offset));
    const q = p.toString();
    return request<{ album: Album; items: Node[] }>(`/api/v1/albums/${id}${q ? `?${q}` : ""}`);
  },

  createAlbum: (name: string, description?: string) =>
    request<{ album: Album }>("/api/v1/albums", {
      method: "POST",
      body: JSON.stringify({ name, description }),
    }),

  updateAlbum: (id: string, patch: { name?: string; description?: string; cover_node_id?: string }) =>
    request<{ album: Album }>(`/api/v1/albums/${id}`, {
      method: "PATCH",
      body: JSON.stringify(patch),
    }),

  deleteAlbum: (id: string) =>
    request<{ status: string }>(`/api/v1/albums/${id}`, { method: "DELETE" }),

  addToAlbum: (id: string, nodeIds: string[]) =>
    request<{ status: string }>(`/api/v1/albums/${id}/items`, {
      method: "POST",
      body: JSON.stringify({ node_ids: nodeIds }),
    }),

  removeFromAlbum: (id: string, nodeId: string) =>
    request<{ status: string }>(`/api/v1/albums/${id}/items/${nodeId}`, { method: "DELETE" }),

  // Replaces the whole order in one call — a drag-reorder that issued N updates
  // would be N chances to end up half-applied.
  reorderAlbum: (id: string, nodeIds: string[]) =>
    request<{ album: Album }>(`/api/v1/albums/${id}/items`, {
      method: "PATCH",
      body: JSON.stringify({ node_ids: nodeIds }),
    }),

  // --- grants & collaboration (Phase 7) -------------------------------------

  // Both directions: what I've shared, and what's been shared with me.
  grants: () => request<{ granted: Grant[]; received: Grant[] }>("/api/v1/grants"),

  grant: (nodeId: string, username: string, role: Role) =>
    request<{ grant: Grant }>(`/api/v1/nodes/${nodeId}/grants`, {
      method: "POST",
      body: JSON.stringify({ username, role }),
    }),

  updateGrant: (grantId: string, role: Role) =>
    request<{ grant: Grant }>(`/api/v1/grants/${grantId}`, {
      method: "PATCH",
      body: JSON.stringify({ role }),
    }),

  revokeGrant: (grantId: string) =>
    request<{ status: string }>(`/api/v1/grants/${grantId}`, { method: "DELETE" }),

  // The roots others have shared with me — the "Shared with me" view.
  shared: () => request<{ items: Node[] }>("/api/v1/shared"),

  // --- admin (Phase 7) — all 403 for non-admins -----------------------------

  adminUsers: () => request<{ users: AdminUser[] }>("/api/v1/admin/users"),

  createUser: (body: { username: string; display_name?: string; is_admin?: boolean }) =>
    request<{ user: AdminUser }>("/api/v1/admin/users", {
      method: "POST",
      body: JSON.stringify(body),
    }),

  updateUser: (
    id: string,
    /** quota_bytes distinguishes three states, and all three are reachable:
     *  absent leaves it alone, a number sets it, and an explicit null clears it
     *  back to unlimited. The server reads the raw body to tell absent from
     *  null, which `number | undefined` alone could not express. */
    patch: {
      display_name?: string;
      is_admin?: boolean;
      disabled?: boolean;
      quota_bytes?: number | null;
    },
  ) =>
    request<{ user: AdminUser }>(`/api/v1/admin/users/${id}`, {
      method: "PATCH",
      body: JSON.stringify(patch),
    }),

  deleteUser: (id: string) =>
    request<{ status: string }>(`/api/v1/admin/users/${id}`, { method: "DELETE" }),

  adminAudit: (opts: { actor?: string; action?: string; limit?: number; offset?: number } = {}) => {
    const p = new URLSearchParams();
    if (opts.actor) p.set("actor", opts.actor);
    if (opts.action) p.set("action", opts.action);
    if (opts.limit != null) p.set("limit", String(opts.limit));
    if (opts.offset != null) p.set("offset", String(opts.offset));
    const q = p.toString();
    return request<{ entries: AuditEntry[]; has_more?: boolean }>(`/api/v1/admin/audit${q ? `?${q}` : ""}`);
  },

  // --- versions -------------------------------------------------------------

  versions: (id: string) => request<{ versions: Version[] }>(`/api/v1/nodes/${id}/versions`),

  restoreVersion: (id: string, versionId: string) =>
    request<{ node: Node }>(`/api/v1/nodes/${id}/versions/${versionId}/restore`, { method: "POST" }),

  // A plain link, like downloadUrl, so the browser streams the past version
  // straight to disk or a player.
  versionDownloadUrl: (id: string, versionId: string, forceDownload = false) =>
    `/api/v1/nodes/${id}/versions/${versionId}/content${forceDownload ? "?download=1" : ""}`,

  // --- shares: management ---------------------------------------------------

  createShare: (
    nodeId: string,
    opts: { password?: string; expiresInHours?: number; maxDownloads?: number } = {},
  ) =>
    request<{ share: CreatedShare }>(`/api/v1/nodes/${nodeId}/shares`, {
      method: "POST",
      body: JSON.stringify({
        password: opts.password ?? "",
        expires_in_hours: opts.expiresInHours ?? 0,
        max_downloads: opts.maxDownloads ?? 0,
      }),
    }),

  shares: () => request<{ shares: ShareInfo[] }>("/api/v1/shares"),

  revokeShare: (id: string) =>
    request<{ status: string }>(`/api/v1/shares/${id}`, { method: "DELETE" }),

  // --- shares: public plane (no session) ------------------------------------

  shareView: (token: string, path = "") =>
    request<ShareView>(`/api/v1/s/${token}${path ? `?path=${encodeURIComponent(path)}` : ""}`),

  unlockShare: (token: string, password: string) =>
    request<{ unlocked: boolean }>(`/api/v1/s/${token}/unlock`, {
      method: "POST",
      body: JSON.stringify({ password }),
    }),

  // A plain link so the browser streams a shared file straight to disk or a
  // player, carrying the unlock cookie automatically.
  shareContentUrl: (token: string, path = "", forceDownload = false) => {
    const params = new URLSearchParams();
    if (path) params.set("path", path);
    if (forceDownload) params.set("download", "1");
    const q = params.toString();
    return `/api/v1/s/${token}/content${q ? `?${q}` : ""}`;
  },
};

// --- formatting -------------------------------------------------------------

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

export function formatDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}
