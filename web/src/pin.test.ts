import { beforeEach, describe, expect, it, vi } from "vitest";

import { isPinned, listPinned, pinFile, supportsPinning, unpinFile } from "./pin";

// An in-memory stand-in for the Cache Storage bucket the service worker serves
// offline. pinFile/unpinFile must keep this bucket and the localStorage index in
// step; the tests assert exactly that pairing.
function installFakeCaches() {
  const stored = new Set<string>();
  const cache = {
    add: vi.fn(async (url: string) => {
      stored.add(url);
    }),
    delete: vi.fn(async (url: string) => stored.delete(url)),
  };
  vi.stubGlobal("caches", { open: vi.fn(async () => cache) });
  return stored;
}

beforeEach(() => {
  localStorage.clear();
  vi.unstubAllGlobals();
});

describe("supportsPinning", () => {
  it("is true when the Cache API exists", () => {
    installFakeCaches();
    expect(supportsPinning()).toBe(true);
  });
});

describe("pin index", () => {
  it("starts empty", () => {
    expect(listPinned()).toEqual([]);
    expect(isPinned("x")).toBe(false);
  });

  it("records a pin in both the cache and the index", async () => {
    const stored = installFakeCaches();
    await pinFile({ id: "a", name: "a.jpg", path: "/a.jpg", size: 10 });

    expect(isPinned("a")).toBe(true);
    expect(listPinned()).toHaveLength(1);
    // The original content URL (no ?variant) is what the SW serves offline.
    expect(stored.has("/api/v1/nodes/a/content")).toBe(true);
  });

  it("lists newest first and never duplicates a re-pin", async () => {
    installFakeCaches();
    await pinFile({ id: "a", name: "a.jpg", path: "/a.jpg" });
    await pinFile({ id: "b", name: "b.jpg", path: "/b.jpg" });
    await pinFile({ id: "a", name: "a.jpg", path: "/a.jpg" }); // re-pin a

    const ids = listPinned().map((f) => f.id);
    expect(ids).toEqual(["a", "b"]);
  });

  it("unpins from both the cache and the index", async () => {
    const stored = installFakeCaches();
    await pinFile({ id: "a", name: "a.jpg", path: "/a.jpg" });
    await unpinFile("a");

    expect(isPinned("a")).toBe(false);
    expect(listPinned()).toEqual([]);
    expect(stored.has("/api/v1/nodes/a/content")).toBe(false);
  });

  it("does not record a phantom pin when the cache write fails", async () => {
    // A failed fetch (offline, or unauthorized) must reject and leave no index
    // entry, so the Offline list never promises bytes that were never stored.
    vi.stubGlobal("caches", {
      open: vi.fn(async () => ({
        add: vi.fn(async () => {
          throw new TypeError("Failed to fetch");
        }),
        delete: vi.fn(async () => false),
      })),
    });

    await expect(pinFile({ id: "z", name: "z.jpg", path: "/z.jpg" })).rejects.toThrow();
    expect(isPinned("z")).toBe(false);
  });

  it("survives a corrupt index without throwing", () => {
    localStorage.setItem("pc-pinned-index", "{not valid json");
    expect(listPinned()).toEqual([]);
    expect(isPinned("a")).toBe(false);
  });
});
