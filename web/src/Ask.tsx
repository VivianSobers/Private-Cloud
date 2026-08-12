import { useState } from "react";

import { api, ApiError, type ChatResponse, type Citation } from "./api";

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
};

export function Ask() {
  const [q, setQ] = useState("");
  const [asked, setAsked] = useState("");
  const [res, setRes] = useState<ChatResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function ask() {
    const question = q.trim();
    if (!question) return;
    setAsked(question);
    setLoading(true);
    setError(null);
    setRes(null);
    try {
      setRes(await api.chat(question, { limit: LIMIT }));
    } catch (e) {
      setError(describe(e));
    } finally {
      setLoading(false);
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
          {res.model && <p className="muted small">answered by {res.model}</p>}
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
