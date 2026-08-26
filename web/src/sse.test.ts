import { describe, expect, it, vi } from "vitest";

import { createSSEParser, parseEventData, readSSEStream, type SSEEvent } from "./sse";

// The framing is the part of streaming that is easy to get subtly wrong and
// hard to notice: a parser that only handles whole frames works perfectly
// against a fast local server and falls apart over a real network, where a
// frame arrives in three pieces. So it is tested here, byte-splitting included,
// rather than through fetch.

/** Collects what a parser dispatches, feeding the text in pieces of `size` so a
 *  chunk boundary is forced everywhere it can fall. */
function parseInChunks(text: string, size: number): SSEEvent[] {
  const seen: SSEEvent[] = [];
  const parser = createSSEParser((e) => seen.push(e));
  for (let i = 0; i < text.length; i += size) parser.push(text.slice(i, i + size));
  parser.end();
  return seen;
}

const stream =
  'event: citations\ndata: {"citations":[{"ref":"1"}]}\n\n' +
  'event: delta\ndata: {"text":"the office "}\n\n' +
  'event: delta\ndata: {"text":"closes at six"}\n\n' +
  'event: done\ndata: {"model":"stub"}\n\n';

describe("frame boundaries", () => {
  it("dispatches whole frames in order", () => {
    const seen = parseInChunks(stream, stream.length);
    expect(seen.map((e) => e.event)).toEqual(["citations", "delta", "delta", "done"]);
  });

  it("reassembles frames split across any chunk boundary", () => {
    // Every size from one character up: if the parser mishandles a split at any
    // position at all, one of these fails.
    for (let size = 1; size <= stream.length; size++) {
      const seen = parseInChunks(stream, size);
      expect(seen.map((e) => e.event), `chunk size ${size}`).toEqual([
        "citations",
        "delta",
        "delta",
        "done",
      ]);
      expect(seen[1]?.data, `chunk size ${size}`).toBe('{"text":"the office "}');
    }
  });

  it("emits nothing until a frame is complete", () => {
    const seen: SSEEvent[] = [];
    const parser = createSSEParser((e) => seen.push(e));
    parser.push('event: delta\ndata: {"text":"half');
    expect(seen).toHaveLength(0);
    parser.push('"}\n\n');
    expect(seen).toHaveLength(1);
  });
});

describe("field parsing", () => {
  it("strips exactly one space after the colon, keeping the rest of the value", () => {
    // Leading whitespace inside a streamed answer is the answer's, not the
    // framing's — trimming it would silently reflow whatever the model wrote.
    const seen = parseInChunks('event: delta\ndata: {"text":"  indented"}\n\n', 4096);
    expect(seen[0]?.data).toBe('{"text":"  indented"}');
  });

  it("joins multi-line data with a newline", () => {
    const seen = parseInChunks("event: delta\ndata: line one\ndata: line two\n\n", 4096);
    expect(seen[0]?.data).toBe("line one\nline two");
  });

  it("defaults an unnamed frame to `message`", () => {
    expect(parseInChunks("data: hello\n\n", 4096)[0]?.event).toBe("message");
  });

  it("accepts a field with no space after the colon", () => {
    const seen = parseInChunks("event:delta\ndata:hello\n\n", 4096);
    expect(seen[0]).toEqual({ event: "delta", data: "hello" });
  });
});

describe("noise on the wire", () => {
  it("ignores heartbeat comments without dispatching an empty event", () => {
    const seen = parseInChunks(
      ":\n\n: keep-alive\n\n" + 'event: done\ndata: {"model":"stub"}\n\n',
      4096,
    );
    expect(seen.map((e) => e.event)).toEqual(["done"]);
  });

  it("drops a frame carrying no data at all", () => {
    expect(parseInChunks("event: delta\n\n", 4096)).toEqual([]);
  });

  it("treats CRLF framing the same as LF", () => {
    const crlf = stream.replace(/\n/g, "\r\n");
    // The server writes LF, but a proxy that rewrites line endings would
    // otherwise turn the whole answer into one frame delivered at the end —
    // indistinguishable from the buffering this feature exists to defeat.
    expect(parseInChunks(crlf, 3).map((e) => e.event)).toEqual([
      "citations",
      "delta",
      "delta",
      "done",
    ]);
  });
});

describe("early termination", () => {
  it("flushes a final frame that never got its blank line", () => {
    const seen = parseInChunks('event: done\ndata: {"model":"stub"}', 4096);
    expect(seen).toHaveLength(1);
    expect(seen[0]?.event).toBe("done");
  });

  it("cannot tell a cut frame from an unterminated one, so the JSON guard does", () => {
    // end() flushes whatever is left, because the honest alternative — dropping
    // it — would lose a final `done` on a connection closed one byte early.
    // What it cannot know is whether that remainder is a whole frame or half of
    // one, so the layer above decides: a frame whose data will not parse is
    // dropped, and the complete frames before it stay on screen.
    const seen = parseInChunks(
      'event: citations\ndata: {"citations":[]}\n\nevent: delta\ndata: {"text":"half',
      4096,
    );
    expect(seen.map((e) => e.event)).toEqual(["citations", "delta"]);
    expect(parseEventData(seen[0]?.data ?? "x")).toEqual({ citations: [] });
    expect(parseEventData(seen[1]?.data ?? "x")).toBeUndefined();
  });

  it("flushes nothing for a stream that carried only whitespace", () => {
    expect(parseInChunks("\n\n\n", 4096)).toEqual([]);
  });
});

describe("parseEventData", () => {
  it("returns the object for a JSON frame", () => {
    expect(parseEventData('{"text":"hi"}')).toEqual({ text: "hi" });
  });

  it("returns undefined for anything that is not a JSON object", () => {
    // Dropped rather than thrown: the rest of the answer is still worth
    // showing, and `done` is what says how the stream ended.
    expect(parseEventData("not json")).toBeUndefined();
    expect(parseEventData("[1,2]")).toBeUndefined();
    expect(parseEventData("null")).toBeUndefined();
  });
});

describe("readSSEStream", () => {
  /** A ReadableStream over the given text pieces, as fetch would deliver them. */
  function bodyOf(...pieces: string[]): ReadableStream<Uint8Array> {
    const encoder = new TextEncoder();
    return new ReadableStream<Uint8Array>({
      start(controller) {
        for (const p of pieces) controller.enqueue(encoder.encode(p));
        controller.close();
      },
    });
  }

  it("decodes and dispatches everything the body carries", async () => {
    const seen: SSEEvent[] = [];
    await readSSEStream(bodyOf(stream.slice(0, 30), stream.slice(30)), (e) => seen.push(e));
    expect(seen.map((e) => e.event)).toEqual(["citations", "delta", "delta", "done"]);
  });

  it("holds back a multi-byte character split across two reads", async () => {
    // An answer in any non-ASCII language would otherwise corrode at exactly
    // the chunk boundaries, which is the kind of bug that only shows up in
    // production and only for some users.
    const bytes = new TextEncoder().encode('event: delta\ndata: {"text":"café"}\n\n');
    const seen: SSEEvent[] = [];
    const split = bytes.length - 5; // between the two bytes of "é"
    await readSSEStream(
      new ReadableStream<Uint8Array>({
        start(c) {
          c.enqueue(bytes.slice(0, split));
          c.enqueue(bytes.slice(split));
          c.close();
        },
      }),
      (e) => seen.push(e),
    );
    expect(seen[0]?.data).toBe('{"text":"café"}');
  });

  it("releases the reader when the stream fails part way", async () => {
    const releaseLock = vi.fn();
    const reader = {
      read: vi.fn().mockRejectedValue(new Error("connection reset")),
      releaseLock,
    };
    const body = { getReader: () => reader } as unknown as ReadableStream<Uint8Array>;
    await expect(readSSEStream(body, () => {})).rejects.toThrow("connection reset");
    expect(releaseLock).toHaveBeenCalled();
  });
});
