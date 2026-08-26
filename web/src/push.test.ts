import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { urlBase64ToUint8Array } from "./push";

describe("urlBase64ToUint8Array", () => {
  // applicationServerKey will not take the base64url string the server serves,
  // so this conversion sits between them and a mistake here fails at
  // subscribe() with a browser error that names nothing useful.
  it("decodes an unpadded base64url key to its raw bytes", () => {
    // "\x04\x01\x02\x03" — an uncompressed-point marker and three bytes, chosen
    // so the encoding needs padding restored.
    expect(Array.from(urlBase64ToUint8Array("BAECAw"))).toEqual([4, 1, 2, 3]);
  });

  it("decodes the URL-safe alphabet rather than standard base64", () => {
    // 0xFB 0xFF encodes as "-_8" in base64url and "+/8" in standard base64. A
    // decoder that forgot the substitution throws instead of returning these.
    expect(Array.from(urlBase64ToUint8Array("-_8"))).toEqual([251, 255]);
  });

  it("round-trips a full 65-byte P-256 point", () => {
    const bytes = new Uint8Array(65);
    bytes[0] = 4;
    for (let i = 1; i < 65; i++) bytes[i] = (i * 7) % 256;

    let binary = "";
    for (const b of bytes) binary += String.fromCharCode(b);
    const base64url = btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

    expect(Array.from(urlBase64ToUint8Array(base64url))).toEqual(Array.from(bytes));
  });
});

describe("push state", () => {
  // The module reads globals at call time, so each test installs the browser
  // surface it wants and removes it afterwards. jsdom provides none of the Push
  // API, which is itself one of the cases worth covering.
  const saved = { ...globalThis } as Record<string, unknown>;

  beforeEach(() => {
    vi.resetModules();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    void saved;
  });

  it("reports unsupported where the browser has no Push API", async () => {
    const { pushSupported, currentState } = await import("./push");
    // jsdom has no PushManager, which is exactly the "old browser" case.
    expect(pushSupported()).toBe(false);
    await expect(currentState()).resolves.toBe("unsupported");
  });

  // A server with no VAPID key configured is a supported deployment, not a
  // failure: the client must report it as "not offered" and keep working.
  it("reports unconfigured when the server publishes no key", async () => {
    vi.stubGlobal("PushManager", class {});
    vi.stubGlobal("Notification", { permission: "default", requestPermission: vi.fn() });
    vi.stubGlobal("navigator", {
      ...navigator,
      serviceWorker: { ready: Promise.resolve({ pushManager: {} }) },
    });

    vi.doMock("./api", () => ({
      api: { pushKey: vi.fn().mockRejectedValue(new Error("404 push_disabled")) },
    }));

    const { currentState } = await import("./push");
    await expect(currentState()).resolves.toBe("unconfigured");
  });

  // Asking on load is how a browser gets trained to block the site forever, so
  // reading the state must never prompt.
  it("does not request permission while merely reporting state", async () => {
    const requestPermission = vi.fn();
    vi.stubGlobal("PushManager", class {});
    vi.stubGlobal("Notification", { permission: "default", requestPermission });
    vi.stubGlobal("navigator", {
      ...navigator,
      serviceWorker: {
        ready: Promise.resolve({ pushManager: { getSubscription: async () => null } }),
      },
    });
    vi.doMock("./api", () => ({
      api: { pushKey: vi.fn().mockResolvedValue({ public_key: "BAECAw" }) },
    }));

    const { currentState } = await import("./push");
    await expect(currentState()).resolves.toBe("prompt");
    expect(requestPermission).not.toHaveBeenCalled();
  });

  it("reports denied without trying to subscribe", async () => {
    vi.stubGlobal("PushManager", class {});
    vi.stubGlobal("Notification", { permission: "denied", requestPermission: vi.fn() });
    vi.stubGlobal("navigator", {
      ...navigator,
      serviceWorker: { ready: Promise.resolve({ pushManager: {} }) },
    });
    vi.doMock("./api", () => ({
      api: { pushKey: vi.fn().mockResolvedValue({ public_key: "BAECAw" }) },
    }));

    const { currentState } = await import("./push");
    await expect(currentState()).resolves.toBe("denied");
  });

  it("registers the subscription against the device it belongs to", async () => {
    const registerPush = vi.fn().mockResolvedValue({ status: "registered" });
    const subscribe = vi.fn().mockResolvedValue({
      toJSON: () => ({ endpoint: "https://push.example/x", keys: { p256dh: "p", auth: "a" } }),
    });

    vi.stubGlobal("PushManager", class {});
    vi.stubGlobal("Notification", {
      permission: "default",
      requestPermission: vi.fn().mockResolvedValue("granted"),
    });
    vi.stubGlobal("navigator", {
      ...navigator,
      serviceWorker: { ready: Promise.resolve({ pushManager: { subscribe } }) },
    });
    vi.doMock("./api", () => ({
      api: { pushKey: vi.fn().mockResolvedValue({ public_key: "BAECAw" }), registerPush },
    }));

    const push = await import("./push");
    await expect(push.subscribe("device-1")).resolves.toBe("subscribed");

    // userVisibleOnly is not optional: browsers revoke a subscription that
    // delivers silently, because it would be a tracking channel.
    expect(subscribe).toHaveBeenCalledWith(
      expect.objectContaining({ userVisibleOnly: true }),
    );
    // Bound to this device, so revoking the device revokes its notifications.
    expect(registerPush).toHaveBeenCalledWith("device-1", expect.objectContaining({
      endpoint: "https://push.example/x",
    }));
  });

  it("does not register anything when permission is refused", async () => {
    const registerPush = vi.fn();
    const subscribe = vi.fn();

    vi.stubGlobal("PushManager", class {});
    vi.stubGlobal("Notification", {
      permission: "default",
      requestPermission: vi.fn().mockResolvedValue("denied"),
    });
    vi.stubGlobal("navigator", {
      ...navigator,
      serviceWorker: { ready: Promise.resolve({ pushManager: { subscribe } }) },
    });
    vi.doMock("./api", () => ({
      api: { pushKey: vi.fn().mockResolvedValue({ public_key: "BAECAw" }), registerPush },
    }));

    const push = await import("./push");
    await expect(push.subscribe("device-1")).resolves.toBe("denied");
    expect(subscribe).not.toHaveBeenCalled();
    expect(registerPush).not.toHaveBeenCalled();
  });

  // Telling the server first means a page closed mid-way leaves a stale local
  // subscription rather than a server that keeps delivering to a dead endpoint.
  it("clears the browser copy even when the server already forgot", async () => {
    const unsubscribeFn = vi.fn().mockResolvedValue(true);
    vi.stubGlobal("PushManager", class {});
    vi.stubGlobal("Notification", { permission: "granted", requestPermission: vi.fn() });
    vi.stubGlobal("navigator", {
      ...navigator,
      serviceWorker: {
        ready: Promise.resolve({
          pushManager: { getSubscription: async () => ({ unsubscribe: unsubscribeFn }) },
        }),
      },
    });
    vi.doMock("./api", () => ({
      api: { unregisterPush: vi.fn().mockRejectedValue(new Error("404")) },
    }));

    const push = await import("./push");
    await expect(push.unsubscribe("device-1")).resolves.toBe("prompt");
    expect(unsubscribeFn).toHaveBeenCalled();
  });
});
