import { useState } from "react";

import { api, ApiError, formatDate, type SearchHit } from "./api";

// "Ask your library": a natural-language question answered by the documents most
// related to it, ranked by meaning. This is the retrieval half — it runs on the
// semantic search the server already has (Phase 4), so unlike the rest of the
// Phase 8 UI it works today. A written, generated answer over these documents is
// a later slice, when a generation endpoint exists; surfacing the source
// documents first is the honest and more trustworthy half anyway.

const LIMIT = 12;

export function Ask() {
  const [q, setQ] = useState("");
  const [asked, setAsked] = useState("");
  const [results, setResults] = useState<SearchHit[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [lexicalFallback, setLexicalFallback] = useState(false);

  async function ask() {
    const question = q.trim();
    if (!question) return;
    setAsked(question);
    setLoading(true);
    setError(null);
    setLexicalFallback(false);
    setResults(null);
    try {
      const res = await api.search(question, { semantic: true, limit: LIMIT });
      setResults(res.results);
    } catch (e) {
      // 503 means the embedding sidecar isn't enabled — fall back to keyword
      // search rather than failing, exactly as the file browser does.
      if (e instanceof ApiError && e.status === 503) {
        try {
          const res = await api.search(question, { limit: LIMIT });
          setLexicalFallback(true);
          setResults(res.results);
        } catch (e2) {
          setError(describe(e2));
        }
      } else {
        setError(describe(e));
      }
    } finally {
      setLoading(false);
    }
  }

  return (
    <section className="stack ask">
      <div className="stack" style={{ gap: "0.25rem" }}>
        <h2 style={{ margin: 0 }}>Ask your library</h2>
        <p className="muted small">
          Ask in plain language — you'll get the documents most related to your
          question, ranked by meaning rather than by keyword.
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

      {lexicalFallback && (
        <div className="banner small">
          Semantic search isn't enabled on this server, so these are keyword
          matches rather than matches by meaning.
        </div>
      )}
      {error && <div className="banner error">{error}</div>}

      {results && <Answers question={asked} hits={results} semantic={!lexicalFallback} />}
    </section>
  );
}

function Answers({
  question,
  hits,
  semantic,
}: {
  question: string;
  hits: SearchHit[];
  semantic: boolean;
}) {
  if (hits.length === 0)
    return (
      <p className="muted">
        Nothing in your library looks related to “{question}”. Try rephrasing, or
        upload the document you're thinking of.
      </p>
    );

  return (
    <div className="stack">
      <p className="muted small">
        {hits.length} most-related {hits.length === 1 ? "document" : "documents"} for
        “{question}”:
      </p>
      <ol className="answer-list">
        {hits.map((h) => (
          <li key={h.id} className="answer-card">
            <div className="row" style={{ alignItems: "baseline" }}>
              <span className="answer-icon" aria-hidden="true">
                {h.kind === "folder" ? "📁" : "📄"}
              </span>
              {h.kind === "file" ? (
                <a className="link answer-name" href={api.downloadUrl(h.id)}>
                  {h.name}
                </a>
              ) : (
                <span className="answer-name">{h.name}</span>
              )}
              <span style={{ flex: 1 }} />
              {semantic && h.semantic && typeof h.score === "number" && (
                <Relevance score={h.score} />
              )}
            </div>
            <div className="muted small answer-path">{h.path}</div>
            <div className="row small answer-meta">
              {h.matched_content && <span className="tag">matched content</span>}
              {h.matched_path && <span className="tag">matched folder</span>}
              <span className="muted">updated {formatDate(h.updated_at)}</span>
            </div>
          </li>
        ))}
      </ol>
    </div>
  );
}

/** A compact bar for cosine similarity in [0,1]. */
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
