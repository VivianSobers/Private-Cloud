import { beforeEach, describe, expect, it, vi } from "vitest";

import { clearInbox, discard, readInbox, shareArrived, supportsSharing } from "./share";

// A stand-in for the Cache Storage bucket the service worker writes shared files
// into. Only the four things share.ts touches are modelled — keys(), match(),
// delete() and caches.delete() — because the point of the module is that a
// share survives a cold start as a Request/Response pair, and a fake that
// reproduced more of the API would be testing the browser instead.
interface Stored {
  url: string;
  body: string;
  headers: Record<string, string>;
}

function installFakeCaches(entries: Stored[]) {
  const store = new Map(entries.map((e) => [e.url, e]));
  const deleted: string[] = [];
  const cache = {
    keys: vi.fn(async () => [...store.keys()].map((url) => ({ url }))),
    match: vi.fn(async (req: { url: string }) => {
      const hit = store.get(req.url);
      if (!hit) return undefined;
      return {
        headers: { get: (h: string) => hit.headers[h] ?? null },
        blob: async () => new Blob([hit.body], { type: hit.headers["Content-Type"] ?? "" }),
      };
    }),
    delete: vi.fn(async (url: string) => store.delete(url)),
  };
  vi.stubGlobal("caches", {
    open: vi.fn(async () => cache),
    delete: vi.fn(async (name: string) => {
      deleted.push(name);
      return true;
    }),
  });
  return { store, deleted };
}

beforeEach(() => {
  vi.unstubAllGlobals();
});

describe("shareArrived", () => {
  it("recognises the redirect the service worker sends", () => {
    expect(shareArrived("?share=inbox")).toBe(true);
    expect(shareArrived("?a=1&share=inbox&b=2")).toBe(true);
  });

  it("is false for an ordinary page load", () => {
    expect(shareArrived("")).toBe(false);
    expect(shareArrived("?share=")).toBe(false);
    expect(shareArrived("?shared=inbox")).toBe(false);
  });
});

describe("supportsSharing", () => {
  it("is true when Cache Storage exists", () => {
    installFakeCaches([]);
    expect(supportsSharing()).toBe(true);
  });

  it("is false without it, and readInbox is then empty rather than throwing", async () => {
    vi.stubGlobal("caches", undefined);
    expect(supportsSharing()).toBe(false);
    await expect(readInbox()).resolves.toEqual([]);
  });
});

describe("readInbox", () => {
  it("is empty when nothing was shared", async () => {
    installFakeCaches([]);
    await expect(readInbox()).resolves.toEqual([]);
  });

  it("recovers the real filename from the header, not from the key", async () => {
    // The key is synthetic precisely because two shares of "photo.jpg" must not
    // overwrite each other; the name a phone allows (spaces, slashes, accents)
    // could never survive being part of it.
    installFakeCaches([
      {
        url: "https://example.test/__share/1700000000000-0",
        body: "jpeg-bytes",
        headers: {
          "Content-Type": "image/jpeg",
          "X-Share-Filename": encodeURIComponent("holiday photo (2)/final.jpg"),
        },
      },
    ]);

    const [item] = await readInbox();
    expect(item?.name).toBe("holiday photo (2)/final.jpg");
    expect(item?.type).toBe("image/jpeg");
    expect(item?.size).toBe("jpeg-bytes".length);
    expect(item?.key).toBe("https://example.test/__share/1700000000000-0");
  });

  it("falls back to the key's last segment when the name is missing or unusable", async () => {
    installFakeCaches([
      { url: "https://example.test/__share/1-0", body: "a", headers: {} },
      // A half-written percent escape: shown as absent rather than raw, because
      // "%E0%A4" is not a filename anybody chose.
      { url: "https://example.test/__share/1-1", body: "b", headers: { "X-Share-Filename": "%E0%A4" } },
    ]);

    const names = (await readInbox()).map((i) => i.name);
    expect(names).toEqual(["1-0", "1-1"]);
  });

  it("assumes a type rather than dropping a file that arrived without one", async () => {
    installFakeCaches([{ url: "https://example.test/__share/2-0", body: "x", headers: {} }]);
    const [item] = await readInbox();
    expect(item?.type).toBe("application/octet-stream");
  });

  it("skips an entry whose body has gone, instead of failing the whole inbox", async () => {
    // A key with no response: eviction under storage pressure between the share
    // arriving and the page opening. One lost file must not hide the others.
    vi.stubGlobal("caches", {
      open: vi.fn(async () => ({
        keys: vi.fn(async () => [
          { url: "https://example.test/__share/3-0" },
          { url: "https://example.test/__share/3-1" },
        ]),
        match: vi.fn(async (req: { url: string }) =>
          req.url.endsWith("3-0")
            ? { headers: { get: () => null }, blob: async () => new Blob(["kept"]) }
            : undefined,
        ),
      })),
    });

    const items = await readInbox();
    expect(items).toHaveLength(1);
    expect(items[0]?.key).toBe("https://example.test/__share/3-0");
  });
});

describe("discard and clearInbox", () => {
  it("removes exactly the item that was dealt with", async () => {
    const { store } = installFakeCaches([
      { url: "https://example.test/__share/4-0", body: "a", headers: {} },
      { url: "https://example.test/__share/4-1", body: "b", headers: {} },
    ]);

    await discard("https://example.test/__share/4-0");

    const keys = (await readInbox()).map((i) => i.key);
    expect(keys).toEqual(["https://example.test/__share/4-1"]);
    expect(store.has("https://example.test/__share/4-0")).toBe(false);
  });

  it("drops the whole bucket when the user is done with the share", async () => {
    const { deleted } = installFakeCaches([
      { url: "https://example.test/__share/5-0", body: "a", headers: {} },
    ]);

    await clearInbox();
    expect(deleted).toEqual(["pc-share-inbox"]);
  });

  it("does nothing at all without Cache Storage", async () => {
    vi.stubGlobal("caches", undefined);
    await expect(discard("anything")).resolves.toBeUndefined();
    await expect(clearInbox()).resolves.toBeUndefined();
  });
});
