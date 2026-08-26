import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { Feedback } from "./api";
import {
  indexVerdicts,
  isRouteAbsent,
  loadFeedback,
  resetFeedback,
  submitVerdict,
  targetKey,
  VERDICTS,
} from "./feedback";

// The pure half of the feedback controls. The three properties worth pinning are
// the ones a bug would be invisible in: that two different targets never share a
// key, that a server without the endpoint degrades to "no controls" rather than
// to an error, and that a refused write does not light the button up anyway.

function jsonResponse(status: number, body: unknown): Response {
  return {
    status,
    ok: status >= 200 && status < 300,
    text: async () => JSON.stringify(body),
  } as Response;
}

function feedback(f: Partial<Feedback>): Feedback {
  return {
    id: "f1",
    kind: "search",
    verdict: "helpful",
    created_at: "2026-08-01T00:00:00Z",
    updated_at: "2026-08-01T00:00:00Z",
    ...f,
  };
}

beforeEach(() => {
  resetFeedback();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("targetKey", () => {
  it("separates targets that differ only in kind", () => {
    expect(targetKey({ kind: "similar", node_id: "n1" })).not.toBe(
      targetKey({ kind: "search", node_id: "n1" }),
    );
  });

  it("separates two answers to different questions", () => {
    expect(targetKey({ kind: "answer", context: "when does it close" })).not.toBe(
      targetKey({ kind: "answer", context: "who signed the lease" }),
    );
  });

  it("ignores surrounding whitespace in the question", () => {
    // The same question typed with a trailing space is the same question, and a
    // second row for it would show the user an unpressed button for something
    // they had already answered.
    expect(targetKey({ kind: "answer", context: " who signed the lease " })).toBe(
      targetKey({ kind: "answer", context: "who signed the lease" }),
    );
  });

  it("does not let a pipe inside a question collide with another target", () => {
    // context is last, so every separator that delimits anything sits between
    // fields that cannot contain one.
    expect(targetKey({ kind: "answer", context: "a|b" })).not.toBe(
      targetKey({ kind: "answer", node_id: "a", context: "b" }),
    );
  });
});

describe("indexVerdicts", () => {
  it("indexes by target", () => {
    const idx = indexVerdicts([
      feedback({ id: "1", kind: "search", node_id: "n1", verdict: "wrong" }),
      feedback({ id: "2", kind: "similar", node_id: "n2", verdict: "helpful" }),
    ]);
    expect(idx[targetKey({ kind: "search", node_id: "n1" })]).toBe("wrong");
    expect(idx[targetKey({ kind: "similar", node_id: "n2" })]).toBe("helpful");
  });

  it("keeps the newest when the same target appears twice", () => {
    // The server returns newest first and stores only one verdict per target, so
    // this should never arise — but if it ever does, the newest is the answer
    // that is still right.
    const idx = indexVerdicts([
      feedback({ id: "new", node_id: "n1", verdict: "helpful" }),
      feedback({ id: "old", node_id: "n1", verdict: "wrong" }),
    ]);
    expect(idx[targetKey({ kind: "search", node_id: "n1" })]).toBe("helpful");
  });

  it("returns nothing for an empty list", () => {
    expect(indexVerdicts([])).toEqual({});
  });
});

describe("loadFeedback", () => {
  it("reports a server without the endpoint as unsupported", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse(404, { error: { code: "not_found", message: "no" } })),
    );
    const state = await loadFeedback();
    // Unsupported means the controls render as absent, not as an error banner.
    expect(state.supported).toBe(false);
  });

  it("stays supported when the failure is not a missing route", async () => {
    // Offline or a 500 must not disable the feature for the rest of the session:
    // the probe is cached, so "absent" would never be reconsidered.
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse(500, { error: { code: "internal", message: "boom" } })),
    );
    const state = await loadFeedback();
    expect(state.supported).toBe(true);
    expect(state.verdicts).toEqual({});
  });

  it("probes once however many controls ask", async () => {
    const fetchMock = vi.fn(async () =>
      jsonResponse(200, { feedback: [feedback({ node_id: "n1", verdict: "wrong" })], count: 1 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const [a, b] = await Promise.all([loadFeedback(), loadFeedback()]);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(a.verdicts[targetKey({ kind: "search", node_id: "n1" })]).toBe("wrong");
    expect(b.supported).toBe(true);
  });
});

describe("submitVerdict", () => {
  it("records the verdict and remembers it for the next control", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init?: RequestInit) =>
        init?.method === "POST"
          ? jsonResponse(201, { feedback: feedback({ node_id: "n1", verdict: "wrong" }) })
          : jsonResponse(200, { feedback: [], count: 0 }),
      ),
    );

    const stored = await submitVerdict({ kind: "search", node_id: "n1" }, "wrong");
    expect(stored).toBe("wrong");

    const state = await loadFeedback();
    expect(state.verdicts[targetKey({ kind: "search", node_id: "n1" })]).toBe("wrong");
  });

  it("returns null when the write was refused, and remembers nothing", async () => {
    // A button that lights up for a write that did not happen tells somebody
    // their correction was taken when it was not.
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init?: RequestInit) =>
        init?.method === "POST"
          ? jsonResponse(404, { error: { code: "not_found", message: "no such file" } })
          : jsonResponse(200, { feedback: [], count: 0 }),
      ),
    );

    expect(await submitVerdict({ kind: "search", node_id: "gone" }, "wrong")).toBeNull();
    const state = await loadFeedback();
    expect(state.verdicts[targetKey({ kind: "search", node_id: "gone" })]).toBeUndefined();
  });
});

describe("isRouteAbsent", () => {
  it("is false for anything that is not an API 404", () => {
    expect(isRouteAbsent(new Error("offline"))).toBe(false);
    expect(isRouteAbsent(undefined)).toBe(false);
  });
});

describe("VERDICTS", () => {
  it("offers exactly the three the server accepts", () => {
    expect(VERDICTS.map((v) => v.value)).toEqual(["helpful", "not_helpful", "wrong"]);
  });

  it("gives every button a name, because the labels are emoji", () => {
    for (const v of VERDICTS) expect(v.title.length).toBeGreaterThan(0);
  });
});
