import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError, api, type Node, type NodeTag } from "./api";

// Tags for one file, in a native <dialog>. Auto tags (from the worker's OCR and
// MIME analysis) sit alongside user tags; a user can add their own and remove
// any — an auto-tagger you cannot correct is worse than none.
export function Tags({ node, onClose }: { node: Node; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const [tags, setTags] = useState<NodeTag[]>([]);
  const [input, setInput] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setTags((await api.nodeTags(node.id)).tags);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, [node.id]);

  useEffect(() => {
    ref.current?.showModal();
    void load();
  }, [load]);

  async function add() {
    const tag = input.trim();
    if (!tag) return;
    setError(null);
    try {
      setTags((await api.addTag(node.id, tag)).tags);
      setInput("");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  async function remove(tag: string) {
    setError(null);
    try {
      await api.removeTag(node.id, tag);
      setTags((ts) => ts.filter((t) => t.name !== tag));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <dialog ref={ref} onClose={onClose} onCancel={onClose}>
      <div className="stack">
        <div className="row">
          <strong>Tags — {node.name}</strong>
          <span style={{ flex: 1 }} />
          <button onClick={onClose}>Close</button>
        </div>

        {error && <div className="banner error">{error}</div>}

        <p className="muted small">
          Tags marked <em>auto</em> were added by the server from the file’s type
          and text. Add your own below; remove any you don’t want.
        </p>

        {loading ? (
          <p className="muted">Loading…</p>
        ) : tags.length === 0 ? (
          <p className="muted small">No tags yet.</p>
        ) : (
          <div className="row" style={{ flexWrap: "wrap", gap: "0.4rem" }}>
            {tags.map((t) => (
              <span key={t.name} className="tag">
                {t.name}
                {t.source === "auto" && <span className="muted small"> · auto</span>}
                <button
                  className="link"
                  aria-label={`Remove tag ${t.name}`}
                  title="Remove"
                  onClick={() => void remove(t.name)}
                >
                  ×
                </button>
              </span>
            ))}
          </div>
        )}

        <div className="row">
          <input
            value={input}
            placeholder="Add a tag…"
            aria-label="Add a tag"
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void add();
            }}
          />
          <button className="primary" disabled={!input.trim()} onClick={() => void add()}>
            Add
          </button>
        </div>
      </div>
    </dialog>
  );
}
