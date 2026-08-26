// Typed client for the private-cloud API.
//
// Every response goes through one place so the error shape is handled once.
// The API returns {error:{code,message,request_id}} for every failure, and
// surfacing request_id to the user is what makes "it broke" answerable from
// the server logs without asking them to reproduce it.

import { parseEventData, readSSEStream } from "./sse";

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

/** A cluster of faces the server believes are the same person. Unnamed until a
 *  user names it; `name` is absent (never "") when nobody has. */
export interface Person {
  id: string;
  face_count: number;
  created_at: string;
  name?: string;
  cover_node_id?: string;
  /** Where the cover face sits in its photo, as [x, y, w, h] fractions (0–1) —
   *  fractions not pixels, so a client crops from whichever variant it has. */
  cover_box?: [number, number, number, number];
}

/** One detected face inside a photo. `box` is [x, y, w, h] in fractions of the
 *  image (0–1). `person_id` is absent when the face belongs to no cluster —
 *  either never assigned, or detached by a user saying "this isn't a face". */
export interface Face {
  id: string;
  box: [number, number, number, number];
  seq: number;
  person_id?: string;
}

/** One document a chat answer drew on. Citations are mandatory — an answer over
 *  your files that can't say which file is unverifiable. */
export interface Citation {
  node_id: string;
  path: string;
  name: string;
  chunk_seq: number;
  score: number;
}

/** The answer to a question over the library. `answer` is present only when a
 *  generator produced one; otherwise `answer_unavailable` says why, and the
 *  citations are the trustworthy retrieval-only result. */
export interface ChatResponse {
  question: string;
  citations: Citation[];
  answer?: string;
  model?: string;
  /** Why there is no answer, or — for generation_truncated — why the answer on
   *  screen stops where it does. Present alongside `answer` in that one case:
   *  the prose already delivered is real and stays, and the note says it is
   *  incomplete rather than pretending the whole thing failed. */
  answer_unavailable?:
    | "no_matching_documents"
    | "generation_disabled"
    | "generation_unavailable"
    | "generation_truncated";
}

/** A file the server judges similar to another, with its similarity score. */
export interface SimilarHit extends Node {
  score?: number;
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

/** A session belonging to another user, as an admin sees it. Same rows as the
 *  self-service session list minus `current` — an admin is never one of them. */
export interface AdminSession {
  id: string;
  kind: string;
  user_agent: string;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
}

/** Platform + storage health for the admin console. Fields the collectors didn't
 *  report are absent rather than zero, so a stale or never-run source reads as
 *  unknown instead of as a confident wrong number. */
export interface AdminStorage {
  /** What the database accounts for across every owner — deliberately NOT pool
   *  capacity; the app knows what it stored, the disks know what they hold. */
  accounted: { stored_bytes: number; trash_bytes: number; file_count: number };
  pools: Array<{
    name: string;
    state: string;
    last_scrub_age_seconds?: number;
    /** Absent when never scrubbed — distinct from false (scrubbed, found errors). */
    last_scrub_clean?: boolean;
    collected_at?: string;
  }>;
  backup: { last_success_at?: string; last_failure_at?: string; age_seconds?: number };
  /** Counts keyed by job state (queued/running/done/failed). */
  jobs: Record<string, number>;
  tiering: { enabled: boolean; note?: string };
  collector: { path: string; available: boolean };
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

/** A synced machine, as opposed to a session. A device is "one of my machines
 *  that is syncing"; it carries a name the user chose and self-reported platform
 *  details. `platform`/`app_version` are absent (never "") when the agent said
 *  nothing. `current` marks the caller's own device so the UI can avoid offering
 *  it "revoke" on the very token making the call. */
export interface Device {
  id: string;
  name: string;
  last_seen_at: string;
  created_at: string;
  expires_at: string;
  has_push: boolean;
  current: boolean;
  platform?: string;
  app_version?: string;
}

/** What a streaming request turned out to be.
 *
 *  A server that does not speak the streaming dialect — an older build, or a
 *  proxy that collapsed the response — answers the same request with one JSON
 *  object. That is not an error, it is the non-streaming behaviour this app
 *  shipped with, so it is reported rather than thrown and the caller renders it
 *  the way it always did.
 */
export type StreamOutcome = { streamed: true } | { streamed: false; body: unknown };

/** Reads a Server-Sent Events response, calling back per event.
 *
 *  fetch rather than EventSource, for two reasons that both matter here:
 *  EventSource cannot issue a POST, and the question is a POST body; and it
 *  reconnects on its own, which for a generation endpoint means silently
 *  spending a GPU again on a request the user never repeated.
 *
 *  Errors before the stream opens are ordinary ApiErrors, so a caller handles a
 *  503 from a server with no embedder exactly as it does for every other call.
 *  The framing itself lives in `sse.ts`, which is where the awkward cases — a
 *  frame split across two reads, a heartbeat comment, CRLF — are unit-tested.
 */
export async function streamSSE(
  path: string,
  body: unknown,
  onEvent: (event: string, data: Record<string, unknown>) => void,
  signal?: AbortSignal,
): Promise<StreamOutcome> {
  const res = await fetch(path, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
    body: JSON.stringify(body),
    signal,
  });

  if (!res.ok) {
    const text = await res.text();
    let parsed: ApiErrorBody | undefined;
    try {
      parsed = JSON.parse(text) as ApiErrorBody;
    } catch {
      throw new ApiError(res.status, "unexpected_response", text.slice(0, 200));
    }
    throw new ApiError(
      res.status,
      parsed?.error?.code ?? "unknown",
      parsed?.error?.message ?? `request failed (${res.status})`,
      parsed?.error?.request_id,
    );
  }

  // The Content-Type is what says whether this server streamed at all. Reading
  // a JSON body with an event parser would produce no events and no error,
  // which is the worst of the three outcomes: a blank pane and nothing to
  // report. Checked before the body is touched, because the two shapes are read
  // in completely different ways.
  const kind = res.headers?.get?.("Content-Type") ?? "";
  if (!kind.toLowerCase().includes("text/event-stream")) {
    const text = await res.text();
    try {
      return { streamed: false, body: JSON.parse(text) as unknown };
    } catch {
      throw new ApiError(res.status, "unexpected_response", text.slice(0, 200));
    }
  }

  if (!res.body) throw new ApiError(502, "unexpected_response", "the response carried no stream");

  await readSSEStream(res.body, ({ event, data }) => {
    const parsed = parseEventData(data);
    if (parsed) onEvent(event, parsed);
  });
  return { streamed: true };
}

/** The three things a streamed answer tells its caller, in the order they can
 *  happen. `onCitations` fires once and always — including on every path where
 *  no answer follows — because the sources are the half of RAG that is
 *  trustworthy on its own. */
export interface ChatStreamHandlers {
  onCitations: (citations: Citation[]) => void;
  onDelta: (text: string) => void;
  onDone: (info: { model?: string; answerUnavailable?: string }) => void;
}

export interface ChatStreamOptions {
  under?: string;
  includeShared?: boolean;
  limit?: number;
  /** Cancels the request. A second question, or leaving the view, must not
   *  leave a first answer still writing itself into the pane. */
  signal?: AbortSignal;
}

/** Replays a whole (non-streamed) chat response through the streaming
 *  handlers, so the view has exactly one rendering path. A complete answer is
 *  one delta: the reader sees the same thing, it simply arrives at once — the
 *  same accommodation the server makes for a generator that cannot stream. */
function emitChatResponse(body: ChatResponse, handlers: ChatStreamHandlers): void {
  handlers.onCitations(body.citations ?? []);
  if (body.answer) handlers.onDelta(body.answer);
  handlers.onDone({ model: body.model, answerUnavailable: body.answer_unavailable });
}

/** Whether a failed streaming attempt is worth retrying without streaming.
 *
 *  The distinction being drawn is "this server does not speak the streaming
 *  dialect" versus "this server said no". A 503 from a deployment with no
 *  embedder would answer the plain request identically, so retrying it spends a
 *  second request to show the same error; a 400 naming the `stream` field, on
 *  the other hand, is precisely how this repository's strict decoder reports an
 *  older build meeting a newer client, and the plain request will work.
 *  Transport failures fall on the retry side: a dropped connection says nothing
 *  about what the server supports.
 */
function streamingUnsupported(e: unknown): boolean {
  if (!(e instanceof ApiError)) return true;
  if (e.status === 400 && /stream/i.test(e.message)) return true;
  if (e.code === "unexpected_response") return true;
  return e.status === 404 || e.status === 405 || e.status === 406 || e.status === 415;
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

  // --- devices (Phase 6) ----------------------------------------------------
  // Which of my machines is syncing, as opposed to which sessions are signed in.

  devices: () => request<{ devices: Device[] }>("/api/v1/devices"),

  renameDevice: (id: string, name: string) =>
    request<{ status: string }>(`/api/v1/devices/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ name }),
    }),

  // Revoke a device token — a lost laptop stops syncing on its next request, not
  // whenever the token would have expired.
  revokeDevice: (id: string) =>
    request<{ status: string }>(`/api/v1/devices/${id}`, { method: "DELETE" }),

  // --- web push (Phase 6) ---------------------------------------------------

  // The VAPID application server key. 404 means this server has no key
  // configured, which is a supported state and not an error: push is a latency
  // optimisation over polling GET /changes, so a client that cannot subscribe
  // simply carries on as every client did before push existed.
  pushKey: () => request<{ public_key: string }>("/api/v1/push/key"),

  registerPush: (id: string, sub: PushSubscriptionJSON) =>
    request<{ status: string }>(`/api/v1/devices/${id}/push`, {
      method: "POST",
      body: JSON.stringify({
        endpoint: sub.endpoint,
        keys: { p256dh: sub.keys?.p256dh, auth: sub.keys?.auth },
      }),
    }),

  unregisterPush: (id: string) =>
    request<{ status: string }>(`/api/v1/devices/${id}/push`, { method: "DELETE" }),

  // --- files ----------------------------------------------------------------

  root: () => request<{ node: Node }>("/api/v1/nodes/root"),

  node: (id: string) => request<{ node: Node }>(`/api/v1/nodes/${id}`),

  // includeShared is the Phase 7 opt-in. Without it this returns exactly what it
  // returned before sharing existed, byte for byte — which is why browsing into
  // somebody else's folder has to ask for it explicitly rather than the default
  // widening under every client that was written before grants.
  children: (id: string, opts: { includeShared?: boolean } = {}) =>
    request<{ parent: Node; children: Node[] }>(
      `/api/v1/nodes/${id}/children${opts.includeShared ? "?include_shared=true" : ""}`,
    ),

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
    opts: {
      under?: string;
      kind?: string;
      limit?: number;
      offset?: number;
      semantic?: boolean;
      includeShared?: boolean;
    } = {},
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
    // Same opt-in as children: searching from inside a shared tree should find
    // what is in it, and searching anywhere else must not change.
    if (opts.includeShared) params.set("include_shared", "true");
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

  tagNodes: (tag: string, opts: { limit?: number; offset?: number; includeShared?: boolean } = {}) => {
    const params = new URLSearchParams();
    if (opts.limit) params.set("limit", String(opts.limit));
    if (opts.offset) params.set("offset", String(opts.offset));
    if (opts.includeShared) params.set("include_shared", "true");
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

  // A user's sessions, seen by an admin — like the self-service list but for
  // another account, with no "current" (the admin is never one of these rows).
  adminUserSessions: (id: string) =>
    request<{ sessions: AdminSession[] }>(`/api/v1/admin/users/${id}/sessions`),

  adminRevokeUserSession: (userId: string, sessionId: string) =>
    request<{ status: string }>(`/api/v1/admin/users/${userId}/sessions/${sessionId}`, {
      method: "DELETE",
    }),

  // Platform + storage health for the console. Reads the same collector files and
  // jobs table the alerts use, so the console and Grafana never disagree.
  adminStorage: () => request<AdminStorage>("/api/v1/admin/storage"),

  adminAudit: (opts: { actor?: string; action?: string; limit?: number; offset?: number } = {}) => {
    const p = new URLSearchParams();
    if (opts.actor) p.set("actor", opts.actor);
    if (opts.action) p.set("action", opts.action);
    if (opts.limit != null) p.set("limit", String(opts.limit));
    if (opts.offset != null) p.set("offset", String(opts.offset));
    const q = p.toString();
    return request<{ entries: AuditEntry[]; has_more?: boolean }>(`/api/v1/admin/audit${q ? `?${q}` : ""}`);
  },

  // --- people, similar, chat (Phase 8) --------------------------------------

  people: () => request<{ people: Person[] }>("/api/v1/people"),

  person: (id: string) => request<{ person: Person; items: Node[] }>(`/api/v1/people/${id}`),

  namePerson: (id: string, name: string) =>
    request<{ person: Person }>(`/api/v1/people/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ name }),
    }),

  mergePerson: (id: string, into: string) =>
    request<{ status: string; into: string }>(`/api/v1/people/${id}/merge`, {
      method: "POST",
      body: JSON.stringify({ into }),
    }),

  deletePerson: (id: string) =>
    request<{ status: string }>(`/api/v1/people/${id}`, { method: "DELETE" }),

  similar: (id: string) =>
    request<{ results: SimilarHit[]; count: number }>(`/api/v1/nodes/${id}/similar`),

  // Who is in this picture. Faces carry a box so the UI can draw them over the
  // photo; an unassigned face is an honest "a face, nobody named" rather than a
  // guess about a real person.
  nodeFaces: (id: string) =>
    request<{ faces: Face[] }>(`/api/v1/nodes/${id}/faces`),

  // Fix one wrong detection: point a face at the right person, or pass null to
  // detach it ("this isn't a face") without deleting the detection.
  reassignFace: (nodeId: string, faceId: string, personId: string | null) =>
    request<{ status: string }>(`/api/v1/nodes/${nodeId}/faces/${faceId}/reassign`, {
      method: "POST",
      body: JSON.stringify({ person_id: personId }),
    }),

  // Ask a question over the library. Retrieval always returns citations; a written
  // answer comes back only when a generator is configured (else answer_unavailable).
  chat: (question: string, opts: { under?: string; includeShared?: boolean; limit?: number } = {}) =>
    request<ChatResponse>("/api/v1/chat", {
      method: "POST",
      body: JSON.stringify({
        question,
        scope: { under: opts.under ?? "" },
        include_shared: opts.includeShared ?? false,
        limit: opts.limit,
      }),
    }),

  // The same question, answered as it is written.
  //
  // The citations arrive first and complete, before a single word of prose —
  // that ordering is the server's contract and the reason streaming is
  // acceptable here at all: the reader always holds every source before there is
  // an answer to check against them. onCitations therefore fires once, and
  // always, including on the paths where no answer follows.
  //
  // Every way this can go wrong ends at the same place the non-streaming view
  // already was, rather than at a broken pane: a server that answers JSON gets
  // replayed through the same handlers, and a stream that dies before rendering
  // anything is re-asked without `stream`. The one case that is NOT retried is a
  // stream that already put prose on screen — the user can see half a paragraph,
  // so it is reported as `generation_truncated` (the server's own word for it,
  // with copy already written for it) instead of being silently replaced by a
  // second, differently-worded answer that also costs the GPU another run.
  chatStream: async (
    question: string,
    handlers: ChatStreamHandlers,
    opts: ChatStreamOptions = {},
  ): Promise<{ streamed: boolean }> => {
    let sawDelta = false;
    let sawDone = false;

    const emit = (event: string, data: Record<string, unknown>): void => {
      if (event === "citations") handlers.onCitations((data.citations as Citation[]) ?? []);
      else if (event === "delta") {
        const text = (data.text as string) ?? "";
        if (text) {
          sawDelta = true;
          handlers.onDelta(text);
        }
      } else if (event === "done") {
        sawDone = true;
        handlers.onDone({
          model: data.model as string | undefined,
          answerUnavailable: data.answer_unavailable as string | undefined,
        });
      }
      // Any other event name is ignored rather than treated as an error: the
      // sequence is allowed to grow, and a client that fails on a name it has
      // not been taught makes it impossible to add one.
    };

    const plain = async (): Promise<{ streamed: boolean }> => {
      emitChatResponse(await api.chat(question, opts), handlers);
      return { streamed: false };
    };

    let outcome: StreamOutcome;
    try {
      outcome = await streamSSE(
        "/api/v1/chat",
        {
          question,
          scope: { under: opts.under ?? "" },
          include_shared: opts.includeShared ?? false,
          limit: opts.limit,
          stream: true,
        },
        emit,
        opts.signal,
      );
    } catch (e) {
      // A cancelled request is not a failure, and must never be retried — the
      // caller asked for it to stop.
      if (opts.signal?.aborted) throw e;
      if (sawDone) return { streamed: true };
      if (sawDelta) {
        handlers.onDone({ answerUnavailable: "generation_truncated" });
        return { streamed: true };
      }
      if (!streamingUnsupported(e)) throw e;
      return plain();
    }

    if (!outcome.streamed) {
      emitChatResponse(outcome.body as ChatResponse, handlers);
      return { streamed: false };
    }
    if (!sawDone) {
      // The stream ended without its terminator. `done` is always sent, so this
      // is a connection that stopped rather than finished — the ambiguity the
      // terminator exists to remove, resolved here on the same rule as a
      // mid-flight error.
      if (sawDelta) {
        handlers.onDone({ answerUnavailable: "generation_truncated" });
        return { streamed: true };
      }
      return plain();
    }
    return { streamed: true };
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
