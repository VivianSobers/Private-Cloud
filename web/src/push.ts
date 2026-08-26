import { api } from "./api";

// Web Push subscription, from the browser's side.
//
// The whole feature is optional twice over: the server may have no VAPID key,
// and the person may not have granted notification permission. Neither is an
// error, and neither changes how the app learns about changes — that is
// GET /changes, which every client polls regardless. Push only makes the app
// find out sooner.
//
// So every function here reports what happened rather than throwing, and the
// caller renders it. A feature that degrades has to degrade legibly, or it
// becomes the thing nobody notices is broken.

export type PushState =
  | "unsupported" // this browser has no Push API
  | "unconfigured" // the server publishes no VAPID key
  | "denied" // the person said no
  | "prompt" // available, not yet asked
  | "subscribed";

/**
 * urlBase64ToUint8Array converts the server's base64url key into the raw bytes
 * PushManager.subscribe wants.
 *
 * applicationServerKey does not accept the base64url string the server serves
 * even though that is the interchange format everything else uses, so this
 * conversion is unavoidable rather than incidental. Padding is re-added because
 * atob rejects unpadded input while the key is transmitted unpadded.
 */
// The return type is pinned to Uint8Array<ArrayBuffer> rather than plain
// Uint8Array: since TypeScript 5.7 the array is generic over its backing buffer,
// and the default ArrayBufferLike admits SharedArrayBuffer, which BufferSource —
// and so applicationServerKey — does not accept. Allocating the buffer
// explicitly is what makes the narrower type true rather than asserted.
export function urlBase64ToUint8Array(base64url: string): Uint8Array<ArrayBuffer> {
  const padded = base64url + "=".repeat((4 - (base64url.length % 4)) % 4);
  const binary = atob(padded.replace(/-/g, "+").replace(/_/g, "/"));
  const out = new Uint8Array(new ArrayBuffer(binary.length));
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}

/** Whether this browser can do push at all. */
export function pushSupported(): boolean {
  return (
    typeof window !== "undefined" &&
    "serviceWorker" in navigator &&
    "PushManager" in window &&
    "Notification" in window
  );
}

/**
 * currentState reports where this browser stands, without asking for anything.
 *
 * Deliberately never prompts: a permission dialog on page load is the pattern
 * that trains people to click "block", and once blocked the browser will not ask
 * again. The prompt belongs behind a button the person chose to press.
 */
export async function currentState(): Promise<PushState> {
  if (!pushSupported()) return "unsupported";

  try {
    await api.pushKey();
  } catch {
    // Any failure to read the key means we cannot subscribe, and the honest
    // report is that the server is not offering push — not that something broke.
    return "unconfigured";
  }

  if (Notification.permission === "denied") return "denied";

  const reg = await navigator.serviceWorker.ready;
  const existing = await reg.pushManager.getSubscription();
  if (existing) return "subscribed";

  return "prompt";
}

/**
 * subscribe asks permission, creates the subscription, and registers it against
 * this device.
 *
 * The device id matters: a subscription is stored against the session it belongs
 * to, so that revoking a device revokes its notifications in the same act. That
 * is also why the server refuses a subscription registered against a sibling's
 * id — otherwise one device could redirect another's notifications.
 */
export async function subscribe(deviceId: string): Promise<PushState> {
  if (!pushSupported()) return "unsupported";

  let key: string;
  try {
    key = (await api.pushKey()).public_key;
  } catch {
    return "unconfigured";
  }

  const permission = await Notification.requestPermission();
  if (permission !== "granted") return permission === "denied" ? "denied" : "prompt";

  const reg = await navigator.serviceWorker.ready;
  // userVisibleOnly is required by every browser that implements this: a
  // subscription that could deliver silently would be a tracking channel, so the
  // platform makes the promise non-optional.
  const sub = await reg.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlBase64ToUint8Array(key),
  });

  await api.registerPush(deviceId, sub.toJSON());
  return "subscribed";
}

/**
 * unsubscribe drops the browser's subscription and the server's copy.
 *
 * The server is told first. If the order were reversed and the page closed in
 * between, the server would keep a subscription the browser has already
 * discarded and would deliver to an endpoint that answers 404 forever — which
 * the sender does eventually clean up, but only after trying. Failing to tell
 * the browser after telling the server just means one redundant local record.
 */
export async function unsubscribe(deviceId: string): Promise<PushState> {
  if (!pushSupported()) return "unsupported";

  try {
    await api.unregisterPush(deviceId);
  } catch {
    // Already gone server-side is the state we wanted; carry on and clear the
    // browser's copy so the two do not disagree.
  }

  const reg = await navigator.serviceWorker.ready;
  const sub = await reg.pushManager.getSubscription();
  if (sub) await sub.unsubscribe();
  return "prompt";
}
