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

## Now that the backend landed ✅

Guru shipped `/chat`, `/people`, and `/nodes/{id}/similar`, so the rest of the
Phase 8 front-end is built against them (shapes verified against the handlers —
no drift):

- **Generated answers** — Ask now calls `POST /chat`: a written answer when a
  generator is configured, always with mandatory **citations** to the source
  documents, degrading to citations-only (`answer_unavailable`) when generation
  is disabled or the sidecar is down.
- **People / faces** ([web/src/People.tsx](../web/src/People.tsx)) — the face
  clusters from `GET /people`, each openable to its photos, with naming via
  `PATCH /people/{id}`.
- **Similar files** — a "Find similar" affordance in the photo viewer
  (`GET /nodes/{id}/similar`) that shows a strip of related files and lets you
  step between them without leaving the lightbox.
