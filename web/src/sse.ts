// A parser for the Server-Sent Events wire format, and nothing else.
//
// It lives in its own module for the same reason `geo.ts` and `share.ts` do: it
// is pure, it is where the fiddly cases live (a frame split across two network
// reads, a heartbeat comment, a line ending nobody chose deliberately), and
// those cases are cheap to test here and expensive to test through `fetch`.
//
// Why hand-rolled rather than `EventSource`: EventSource can only issue a GET,
// and the question this app streams is a POST body. It also reconnects on its
// own, which against a generation endpoint means silently spending a GPU again
// on a request the user never repeated. So the transport is `fetch` plus a
// ReadableStream reader, and this module is the part that turns bytes into
// events.

/** One dispatched event: the `event:` name and the joined `data:` payload. */
export interface SSEEvent {
  /** The `event:` field, or "message" when the frame did not name one — the
   *  default the specification gives. */
  event: string;
  /** Every `data:` line of the frame, joined with a newline, exactly as the
   *  specification prescribes. The server here sends one line of JSON, but a
   *  generated answer containing a newline is one sidecar change away from
   *  arriving as several. */
  data: string;
}

/** A push parser: feed it decoded text as it arrives, in whatever pieces the
 *  network happened to deliver, and it calls back once per complete frame. */
export interface SSEParser {
  /** Feeds one chunk of decoded text. Any incomplete trailing frame is held
   *  back until the rest of it arrives. */
  push(chunk: string): void;
  /** Flushes a final frame that arrived without its terminating blank line. */
  end(): void;
}

export function createSSEParser(onEvent: (event: SSEEvent) => void): SSEParser {
  // Whatever has been received and not yet dispatched. A chunk boundary can
  // land anywhere — mid-frame, mid-line, even between the \r and the \n of one
  // line ending — so nothing is parsed until a frame is known to be complete.
  let buffer = "";

  const dispatch = (frame: string): void => {
    let event = "";
    const data: string[] = [];

    for (const line of frame.split("\n")) {
      // A line starting with a colon is a comment. Servers and proxies send
      // these as heartbeats to keep an idle connection from being reaped, and
      // they must be ignored rather than treated as an empty event.
      if (line === "" || line.startsWith(":")) continue;

      const colon = line.indexOf(":");
      const field = colon === -1 ? line : line.slice(0, colon);
      // A single leading space after the colon is part of the framing, not part
      // of the value — and only one. Trimming further would silently eat the
      // indentation of a streamed answer.
      let value = colon === -1 ? "" : line.slice(colon + 1);
      if (value.startsWith(" ")) value = value.slice(1);

      if (field === "event") event = value;
      else if (field === "data") data.push(value);
      // `id` and `retry` are deliberately ignored: both exist to serve
      // reconnection, and this client never reconnects.
    }

    // A frame carrying no data is not dispatched. That is the specification's
    // rule, and it is also what keeps a run of heartbeats from being reported
    // as a stream of empty events.
    if (data.length === 0) return;
    onEvent({ event: event || "message", data: data.join("\n") });
  };

  const drain = (): void => {
    for (;;) {
      // Frames are separated by a blank line, which may be written with either
      // line ending — the server here uses \n, but a proxy that rewrites them
      // would otherwise turn the whole stream into one frame delivered at the
      // end, which looks exactly like the buffering this feature exists to
      // defeat.
      const lf = buffer.indexOf("\n\n");
      const crlf = buffer.indexOf("\r\n\r\n");
      let at: number;
      let width: number;
      if (lf !== -1 && (crlf === -1 || lf < crlf)) {
        at = lf;
        width = 2;
      } else if (crlf !== -1) {
        at = crlf;
        width = 4;
      } else {
        return;
      }
      dispatch(normalize(buffer.slice(0, at)));
      buffer = buffer.slice(at + width);
    }
  };

  return {
    push(chunk: string): void {
      if (!chunk) return;
      buffer += chunk;
      drain();
    },
    end(): void {
      const rest = normalize(buffer).replace(/^\n+/, "");
      buffer = "";
      // The server always terminates its last frame, so this is for the case it
      // cannot control: a connection closed on the byte after the final `data:`
      // line. Losing the `done` event there would leave the UI unable to tell a
      // finished answer from a dropped one — the exact ambiguity the `done`
      // event exists to remove.
      if (rest.trim() !== "") dispatch(rest);
    },
  };
}

/** Collapses \r\n and bare \r to \n so the line splitting has one case. */
function normalize(frame: string): string {
  return frame.replace(/\r\n?/g, "\n");
}

/** Parses a frame's `data` as JSON, returning undefined when it is not an
 *  object. A malformed frame is dropped rather than killing the stream: the
 *  rest of the answer is still worth showing, and `done` is what tells the
 *  caller how the stream ended. */
export function parseEventData(data: string): Record<string, unknown> | undefined {
  try {
    const parsed: unknown = JSON.parse(data);
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return undefined;
    return parsed as Record<string, unknown>;
  } catch {
    return undefined;
  }
}

/** Reads a fetch response body to completion, dispatching each event as it
 *  arrives. Split out from the request itself so the parser and the reader can
 *  both be tested without a network. */
export async function readSSEStream(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: SSEEvent) => void,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  const parser = createSSEParser(onEvent);
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      // stream: true so a multi-byte character split across two reads is held
      // back rather than decoded into a replacement character — an answer in
      // any non-ASCII language would otherwise corrode at chunk boundaries.
      if (value) parser.push(decoder.decode(value, { stream: true }));
    }
    parser.push(decoder.decode());
    parser.end();
  } finally {
    // Releasing matters on the abort path: the reader outlives the loop when
    // the caller cancels, and a lock left held keeps the response body alive.
    reader.releaseLock();
  }
}
