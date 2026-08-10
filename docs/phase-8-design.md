# Phase 8 — Advanced intelligence (Vivian's front-of-API) design

**Status: the runnable half shipped.** Unlike Phases 5 and 7, whose UIs wait on
new server endpoints, Phase 8 has one piece that works on the server *today*.

## Ask your library ([web/src/Ask.tsx](../web/src/Ask.tsx)) ✅ — runs now

A natural-language question answered by the documents most related to it, ranked
by meaning. It is the **retrieval** half of "chat over your documents", and it
runs entirely on the semantic search the server already shipped in Phase 4
(`GET /search?semantic=true`) — no new endpoint required.

- Type a question; the view returns the top related documents with a relevance
  bar (cosine similarity), content/folder match badges, and a link to open each.
- If the embedding sidecar is disabled the endpoint answers `503`; the view falls
  back to keyword search and says so, exactly as the file browser does.
- Surfacing the **source documents** first is deliberate: it is the trustworthy
  half of RAG, and it is useful on its own. A written, generated answer over
  these documents is a later slice — it needs a generation endpoint (a model on a
  4090), which does not exist yet.

## Not yet (need new server surface)

- **Generated answers** — `POST /ask` (or `/chat`): retrieve, then have a model
  compose an answer citing the documents above. Front-end plugs into the same
  results this view already renders.
- **People / faces** — a face-clustering job and `GET /people`; a "name this
  face" UI.
- **Similar files** — a near-duplicate endpoint; a "more like this" affordance in
  the browser and gallery.

Each of these is additive and lands in the contract first, like every other
cross-track feature.
