import { afterEach, describe, expect, it, vi } from "vitest";

import { api, ApiError, formatBytes, formatDate } from "./api";

// A minimal Response-like stand-in. The real fetch is stubbed per test so the
// request layer is exercised end to end — status handling, JSON parsing, and the
// ApiError shape a caller depends on — without a network.
function jsonResponse(status: number, body: unknown): Response {
  const text = typeof body === "string" ? body : JSON.stringify(body);
  return {
    status,
    ok: status >= 200 && status < 300,
    text: async () => text,
  } as Response;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("formatBytes", () => {
  it("keeps small values in bytes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1023)).toBe("1023 B");
  });

  it("steps up units and keeps one decimal below ten", () => {
    expect(formatBytes(1024)).toBe("1.0 KiB");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(1024 ** 2)).toBe("1.0 MiB");
    expect(formatBytes(1024 ** 3)).toBe("1.0 GiB");
  });

  it("drops the decimal at ten units and above", () => {
    expect(formatBytes(10 * 1024)).toBe("10 KiB");
    expect(formatBytes(15 * 1024 * 1024)).toBe("15 MiB");
  });

  it("saturates at the largest unit rather than inventing one", () => {
    expect(formatBytes(5 * 1024 ** 5)).toBe("5.0 PiB");
    expect(formatBytes(5000 * 1024 ** 5)).toBe("5000 PiB");
  });
});

describe("formatDate", () => {
  it("returns an empty string for an unparseable date", () => {
    expect(formatDate("not-a-date")).toBe("");
    expect(formatDate("")).toBe("");
  });

  it("renders a valid ISO timestamp to a non-empty string", () => {
    expect(formatDate("2026-08-16T12:00:00Z")).not.toBe("");
  });
});

describe("request error handling", () => {
  it("parses a structured API error into ApiError fields", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(404, {
          error: { code: "not_found", message: "no such node", request_id: "req-123" },
        }),
      ),
    );

    const err = await api.me().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    const e = err as ApiError;
    expect(e.status).toBe(404);
    expect(e.code).toBe("not_found");
    expect(e.message).toBe("no such node");
    expect(e.requestId).toBe("req-123");
  });

  it("falls back to a generic code when the body has no error shape", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(500, {})));
    const e = (await api.me().catch((x: unknown) => x)) as ApiError;
    expect(e).toBeInstanceOf(ApiError);
    expect(e.code).toBe("unknown");
    expect(e.status).toBe(500);
  });

  it("reports a non-JSON upstream error as unexpected_response", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(502, "<html>Bad Gateway</html>")));
    const e = (await api.me().catch((x: unknown) => x)) as ApiError;
    expect(e).toBeInstanceOf(ApiError);
    expect(e.code).toBe("unexpected_response");
    expect(e.status).toBe(502);
  });

  it("rejects a 2xx with an unparseable body rather than returning undefined", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(200, "not json at all")));
    const e = (await api.me().catch((x: unknown) => x)) as ApiError;
    expect(e).toBeInstanceOf(ApiError);
    expect(e.code).toBe("unexpected_response");
  });
});

describe("request success", () => {
  it("returns the parsed JSON body on 200", async () => {
    const me = { user: { id: "u1", username: "vivian", is_admin: false } };
    const fetchMock = vi.fn(async () => jsonResponse(200, me));
    vi.stubGlobal("fetch", fetchMock);

    const out = await api.me();
    expect(out).toEqual(me);
    // Same-origin so the session cookie is sent; asserting it guards a regression
    // that would silently sign every request out.
    expect(fetchMock).toHaveBeenCalledWith("/api/v1/auth/me", expect.objectContaining({ credentials: "same-origin" }));
  });
});

describe("the shared-content opt-in", () => {
  // ?include_shared=true widens what a listing MEANS without changing its shape,
  // so the rule is that it is sent only when asked for. These assert both halves
  // of that: absent by default, present on request.
  function captureUrl(): { calls: string[] } {
    const calls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        calls.push(url);
        return jsonResponse(200, {});
      }),
    );
    return { calls };
  }

  it("omits include_shared from a listing by default", async () => {
    const seen = captureUrl();
    await api.children("abc");
    expect(seen.calls[0]).toBe("/api/v1/nodes/abc/children");
  });

  it("sends include_shared on a listing when the caller opts in", async () => {
    const seen = captureUrl();
    await api.children("abc", { includeShared: true });
    expect(seen.calls[0]).toBe("/api/v1/nodes/abc/children?include_shared=true");
  });

  it("omits include_shared from a search by default", async () => {
    const seen = captureUrl();
    await api.search("invoices");
    expect(seen.calls[0]).not.toContain("include_shared");
  });

  it("sends include_shared on a search when the caller opts in", async () => {
    const seen = captureUrl();
    await api.search("invoices", { includeShared: true });
    expect(seen.calls[0]).toContain("include_shared=true");
  });

  it("sends include_shared on a tag listing when the caller opts in", async () => {
    const seen = captureUrl();
    await api.tagNodes("receipts", { includeShared: true });
    expect(seen.calls[0]).toContain("include_shared=true");
  });

  it("omits include_shared from a tag listing by default", async () => {
    const seen = captureUrl();
    await api.tagNodes("receipts");
    expect(seen.calls[0]).toBe("/api/v1/tags/receipts");
  });

  it("keeps the opt-in when a tag listing pages", async () => {
    // Page two of a shared tag listing must be as wide as page one, or the
    // second page silently drops back to owner-only halfway down the list.
    const seen = captureUrl();
    await api.tagNodes("receipts", { limit: 50, offset: 50, includeShared: true });
    expect(seen.calls[0]).toContain("offset=50");
    expect(seen.calls[0]).toContain("include_shared=true");
  });

  it("sends nothing but the query when a search is neither scoped nor widened", async () => {
    // The compatibility property in one assertion: a default search is the
    // request a pre-Phase-7 client made, character for character.
    const seen = captureUrl();
    await api.search("invoices");
    expect(seen.calls[0]).toBe("/api/v1/search?q=invoices");
  });
});

describe("contentUrl", () => {
  it("omits the variant query for the original", () => {
    expect(api.contentUrl("abc")).toBe("/api/v1/nodes/abc/content");
  });
  it("appends the variant when asked", () => {
    expect(api.contentUrl("abc", "thumb")).toBe("/api/v1/nodes/abc/content?variant=thumb");
  });
});
