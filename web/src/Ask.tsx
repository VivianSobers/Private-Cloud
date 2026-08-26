import { useEffect, useRef, useState } from "react";

import { api, ApiError, type ChatResponse, type Citation } from "./api";
import { FeedbackControls } from "./FeedbackControls";

// "Ask your library": a natural-language question answered over your own
// documents. Retrieval always returns the source documents (citations); when the
// server has a generator configured, it also returns a written answer grounded in
// them. Either way the citations are shown — an answer over your files that can't
// say which file it came from is not one worth trusting.

const LIMIT = 12;

const UNAVAILABLE: Record<string, string> = {
  no_matching_documents: "Nothing in your library looks related to that.",
  generation_disabled:
    "A written answer isn't enabled on this server — here are the most relevant documents.",
  generation_unavailable:
    "The answer generator is unavailable right now — here are the most relevant documents.",
  generation_truncated:
    "The answer stopped part way through — the generator failed mid-sentence. What's above is what it managed; the citations below are complete.",
};

export function Ask() {
  const [q, setQ] = useState("");
  const [asked, setAsked] = useState("");
  const [res, setRes] = useState<ChatResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The request in flight, so a second question cancels the first rather than
  // racing it. Without this, two answers write into one pane in whatever order
  // the sidecar happens to finish them — and on a box where an answer takes
  // tens of seconds, asking again before the first finishes is the normal way
  // to use this view, not an edge case.
  const inFlight = useRef<AbortController | null>(null);

  // Leaving the view aborts too: an answer still arriving into a component
  // nobody is looking at is a GPU busy for no one.
  useEffect(() => () => inFlight.current?.abort(), []);

  async function ask() {
    const question = q.trim();
    if (!question) return;

    inFlight.current?.abort();
    const controller = new AbortController();
    inFlight.current = controller;

    setAsked(question);
    setLoading(true);
    setError(null);
    setRes(null);

    // Every state update is gated on this request still being the current one.
    // An aborted fetch rejects asynchronously, so a superseded call can still
    // be holding a delta when its replacement has already painted.
    const live = () => inFlight.current === controller && !controller.signal.aborted;

    try {
      // The citations land first and complete — the server writes them before a
      // single word of prose — so the sources are on screen while the answer is
      // still being written, and the reader is never looking at an unattributed
      // paragraph. Each delta is appended to the answer already rendered.
      //
      // A server that cannot stream, or a stream that breaks before rendering
      // anything, is replayed through these same handlers by `chatStream`, so
      // this view has one rendering path and degrades to the behaviour it
      // shipped with rather than to an empty pane.
      await api.chatStream(
        question,
        {
          onCitations: (citations) => {
            if (live()) setRes({ question, citations });
          },
          onDelta: (text) => {
            if (!live()) return;
            setRes((cur) =>
              cur ? { ...cur, answer: (cur.answer ?? "") + text } : { question, citations: [], answer: text },
            );
          },
          onDone: (info) => {
            if (!live()) return;
            setRes((cur) =>
              cur
                ? {
                    ...cur,
                    model: info.model,
                    answer_unavailable: info.answerUnavailable as ChatResponse["answer_unavailable"],
                  }
                : cur,
            );
          },
        },
        { limit: LIMIT, signal: controller.signal },
      );
    } catch (e) {
      // A cancelled request is not a failure to report: the user either asked
      // something else or left, and both already show what they asked for.
      if (live()) setError(describe(e));
    } finally {
      if (inFlight.current === controller) {
        inFlight.current = null;
        setLoading(false);
      }
    }
  }

  return (
    <section className="stack ask">
      <div className="stack" style={{ gap: "0.25rem" }}>
        <h2 style={{ margin: 0 }}>Ask your library</h2>
        <p className="muted small">
          Ask in plain language. You'll get a written answer where available, always
          grounded in — and linked to — the documents it came from.
        </p>
      </div>

      <div className="row">
        <input
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="e.g. what did the plumber quote to replace the boiler?"
          style={{ flex: 1 }}
          onKeyDown={(e) => e.key === "Enter" && void ask()}
          aria-label="question"
        />
        <button className="primary" onClick={() => void ask()} disabled={loading}>
          {loading ? "Thinking…" : "Ask"}
        </button>
      </div>

      {error && <div className="banner error">{error}</div>}
      {res && <Answer question={asked} res={res} />}
    </section>
  );
}

function Answer({ question, res }: { question: string; res: ChatResponse }) {
  const note = res.answer_unavailable ? UNAVAILABLE[res.answer_unavailable] : null;

  return (
    <div className="stack">
      {res.answer ? (
        <div className="chat-answer">
          <p style={{ whiteSpace: "pre-wrap", margin: 0 }}>{res.answer}</p>
          <div className="row" style={{ alignItems: "baseline" }}>
            {res.model && <p className="muted small">answered by {res.model}</p>}
            <span style={{ flex: 1 }} />
            {/* An answer is not a row anywhere, so the question identifies it. */}
            <FeedbackControls
              target={{ kind: "answer", context: question }}
              label="feedback on this answer"
            />
          </div>
        </div>
      ) : (
        <p className="muted">{note ?? "No answer."}</p>
      )}

      {res.citations.length > 0 && (
        <div className="stack">
          <p className="muted small">
            {res.answer ? "Sources" : `Most related to “${question}”`}:
          </p>
          <ol className="answer-list">
            {res.citations.map((c, i) => (
              <CitationCard key={`${c.node_id}-${c.chunk_seq}-${i}`} c={c} />
            ))}
          </ol>
        </div>
      )}
    </div>
  );
}

function CitationCard({ c }: { c: Citation }) {
  return (
    <li className="answer-card">
      <div className="row" style={{ alignItems: "baseline" }}>
        <span className="answer-icon" aria-hidden="true">
          📄
        </span>
        <a className="link answer-name" href={api.downloadUrl(c.node_id)}>
          {c.name}
        </a>
        <span style={{ flex: 1 }} />
        {typeof c.score === "number" && <Relevance score={c.score} />}
        {/* Per citation, not only per answer. "The answer was wrong" and "this
            particular document should not have been cited" are different
            complaints, and only the second one the server can act on: a citation
            marked wrong stops being retrieved for this person. */}
        <FeedbackControls
          target={{ kind: "citation", node_id: c.node_id }}
          label={`feedback on ${c.name}`}
        />
      </div>
      <div className="muted small answer-path">{c.path}</div>
    </li>
  );
}

/** A compact bar for a similarity score in [0,1]. */
function Relevance({ score }: { score: number }) {
  const pct = Math.max(0, Math.min(100, Math.round(score * 100)));
  return (
    <span className="relevance" title={`relevance ${pct}%`} aria-label={`relevance ${pct}%`}>
      <span className="relevance-bar" style={{ width: `${pct}%` }} />
    </span>
  );
}

function describe(e: unknown): string {
  if (e instanceof ApiError) return `${e.code}: ${e.message}${e.requestId ? ` (${e.requestId})` : ""}`;
  return e instanceof Error ? e.message : "Unknown error";
}
