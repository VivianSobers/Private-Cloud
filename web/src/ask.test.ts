import { afterEach, describe, expect, it, vi } from "vitest";

import { api, ApiError, type Citation } from "./api";

// The streaming half of "Ask your library", exercised where the repo tests this
// layer: through the API client with fetch stubbed, not through a rendered
// component. What Ask.tsx does with these callbacks is append text to a pane;
// what is worth asserting is the contract underneath it — citations first, prose
// after, a terminator always, and a degraded path that never leaves the view
// with nothing.

afterEach(() => {
  vi.unstubAllGlobals();
});

/** Collects the handler calls in the order they happen, which is the property
 *  most of these tests are about. */
function recorder() {
  const order: string[] = [];
  let citations: Citation[] = [];
  let answer = "";
  let done: { model?: string; answerUnavailable?: string } | undefined;
  return {
    order,
    get citations() {
      return citations;
    },
    get answer() {
      return answer;
    },
    get done() {
      return done;
    },
    handlers: {
      onCitations: (c: Citation[]) => {
        order.push("citations");
        citations = c;
      },
      onDelta: (t: string) => {
        order.push("delta");
        answer += t;
      },
      onDone: (info: { model?: string; answerUnavailable?: string }) => {
        order.push("done");
        done = info;
      },
    },
  };
}

/** A fetch stubbed with an event stream, delivered in the pieces given so a
 *  test can decide where the network boundaries fall. */
function stubStream(...pieces: string[]) {
  const encoder = new TextEncoder();
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => {
    return {
      ok: true,
      status: 200,
      headers: new Headers({ "Content-Type": "text/event-stream" }),
      body: new ReadableStream<Uint8Array>({
        start(c) {
          for (const p of pieces) c.enqueue(encoder.encode(p));
          c.close();
        },
      }),
    } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

const CITATIONS =
  '{"question":"q","citations":[{"ref":"1","node_id":"n1","path":"/handbook.txt","name":"handbook.txt","chunk_seq":0,"score":0.9}]}';

describe("a streamed answer", () => {
  it("asks with stream:true and reassembles the deltas in order", async () => {
    const fetchMock = stubStream(
      `event: citations\ndata: ${CITATIONS}\n\n`,
      'event: delta\ndata: {"text":"the office "}\n\n',
      'event: delta\ndata: {"text":"closes at six [1]"}\n\n',
      'event: done\ndata: {"model":"stub-streamer"}\n\n',
    );
    const r = recorder();

    const out = await api.chatStream("when does the office close", r.handlers, { limit: 12 });

    expect(out).toEqual({ streamed: true });
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit | undefined;
    expect(JSON.parse(String(init?.body))).toMatchObject({
      question: "when does the office close",
      stream: true,
      limit: 12,
    });
    expect(r.answer).toBe("the office closes at six [1]");
    expect(r.done).toEqual({ model: "stub-streamer", answerUnavailable: undefined });
  });

  it("delivers every citation before the first word of prose", async () => {
    // The property the whole feature turns on. A reader must never be looking
    // at a confident paragraph with nothing yet saying where it came from.
    stubStream(
      `event: citations\ndata: ${CITATIONS}\n\n` +
        'event: delta\ndata: {"text":"answer"}\n\n' +
        'event: done\ndata: {"model":"m"}\n\n',
    );
    const r = recorder();

    await api.chatStream("q", r.handlers);

    expect(r.order).toEqual(["citations", "delta", "done"]);
    expect(r.citations).toHaveLength(1);
  });

  it("survives a frame split across two network reads", async () => {
    const whole =
      `event: citations\ndata: ${CITATIONS}\n\n` +
      'event: delta\ndata: {"text":"answer"}\n\n' +
      'event: done\ndata: {"model":"m"}\n\n';
    stubStream(whole.slice(0, 40), whole.slice(40, 120), whole.slice(120));
    const r = recorder();

    await api.chatStream("q", r.handlers);

    expect(r.order).toEqual(["citations", "delta", "done"]);
    expect(r.answer).toBe("answer");
  });

  it("ignores heartbeats and event names it has not been taught", async () => {
    stubStream(
      `: keep-alive\n\nevent: citations\ndata: ${CITATIONS}\n\n` +
        'event: progress\ndata: {"stage":"retrieval"}\n\n' +
        'event: done\ndata: {"answer_unavailable":"generation_disabled"}\n\n',
    );
    const r = recorder();

    await api.chatStream("q", r.handlers);

    expect(r.order).toEqual(["citations", "done"]);
  });
});

describe("degraded modes keep their message", () => {
  // Each of these is a mode the view already has copy for, and the streamed
  // path must report it by exactly the same name the non-streaming one did.
  for (const reason of [
    "no_matching_documents",
    "generation_disabled",
    "generation_unavailable",
    "generation_truncated",
  ]) {
    it(`passes ${reason} straight through`, async () => {
      stubStream(
        `event: citations\ndata: ${CITATIONS}\n\n` +
          `event: done\ndata: {"answer_unavailable":"${reason}"}\n\n`,
      );
      const r = recorder();

      await api.chatStream("q", r.handlers);

      expect(r.done?.answerUnavailable).toBe(reason);
      expect(r.citations).toHaveLength(1);
    });
  }
});

/** A plain JSON response, the shape POST /chat returns without `stream`. */
function jsonResponse(status: number, body: unknown, contentType = "application/json"): Response {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers({ "Content-Type": contentType }),
    text: async () => text,
  } as Response;
}

const WHOLE = {
  question: "q",
  citations: [
    { ref: "1", node_id: "n1", path: "/h.txt", name: "h.txt", chunk_seq: 0, score: 0.9 },
  ],
  answer: "a whole answer",
  model: "stub",
};

describe("falling back to the non-streaming answer", () => {
  it("renders a server that answered JSON instead of an event stream", async () => {
    // Not an error: it is the behaviour this view shipped with. The whole
    // answer becomes one delta, so there is a single rendering path.
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(200, WHOLE)));
    const r = recorder();

    const out = await api.chatStream("q", r.handlers);

    expect(out).toEqual({ streamed: false });
    expect(r.order).toEqual(["citations", "delta", "done"]);
    expect(r.answer).toBe("a whole answer");
    expect(r.done?.model).toBe("stub");
  });

  it("re-asks without streaming when an older server refuses the field", async () => {
    // A strict decoder answers 400 naming the field it did not recognise, which
    // is exactly how an older build meets a newer client.
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(400, { error: { code: "bad_request", message: 'unknown field "stream"' } }),
      )
      .mockResolvedValueOnce(jsonResponse(200, WHOLE));
    vi.stubGlobal("fetch", fetchMock);
    const r = recorder();

    const out = await api.chatStream("q", r.handlers);

    expect(out).toEqual({ streamed: false });
    const retry = JSON.parse(String((fetchMock.mock.calls[1]?.[1] as RequestInit | undefined)?.body));
    expect(retry).not.toHaveProperty("stream");
    expect(r.answer).toBe("a whole answer");
  });

  it("re-asks when the connection fails before anything was rendered", async () => {
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new TypeError("network error"))
      .mockResolvedValueOnce(jsonResponse(200, WHOLE));
    vi.stubGlobal("fetch", fetchMock);
    const r = recorder();

    await api.chatStream("q", r.handlers);

    expect(r.answer).toBe("a whole answer");
  });

  it("reports a stream that died after prose as truncated, and does not re-ask", async () => {
    // The user can see half a paragraph. Replacing it with a second,
    // differently-worded answer would be a worse lie than saying it stopped —
    // and it would spend the GPU again.
    const fetchMock = stubStream(
      `event: citations\ndata: ${CITATIONS}\n\n`,
      'event: delta\ndata: {"text":"half an "}\n\n',
    );
    const r = recorder();

    const out = await api.chatStream("q", r.handlers);

    expect(out).toEqual({ streamed: true });
    expect(r.answer).toBe("half an ");
    expect(r.done?.answerUnavailable).toBe("generation_truncated");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("does not re-ask a server that said no for a reason of its own", async () => {
    // A 503 from a deployment with no embedder would answer the plain request
    // identically, so retrying spends a request to show the same error twice.
    const fetchMock = vi.fn(async () =>
      jsonResponse(503, {
        error: {
          code: "semantic_unavailable",
          message: "asking questions needs the embedding sidecar",
        },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const err = await api.chatStream("q", recorder().handlers).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe("semantic_unavailable");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe("cancellation", () => {
  it("passes the signal through and never retries a cancelled question", async () => {
    // A second question aborts the first. Retrying it non-streamed would resume
    // work the user explicitly abandoned.
    const controller = new AbortController();
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      expect(init?.signal).toBe(controller.signal);
      controller.abort();
      const err = new Error("The operation was aborted.");
      err.name = "AbortError";
      return Promise.reject(err);
    });
    vi.stubGlobal("fetch", fetchMock);

    const r = recorder();
    const err = await api
      .chatStream("q", r.handlers, { signal: controller.signal })
      .catch((e: unknown) => e);

    expect((err as Error).name).toBe("AbortError");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(r.order).toEqual([]);
  });
});
