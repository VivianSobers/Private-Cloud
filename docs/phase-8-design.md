# Phase 8 — Advanced intelligence (Vivian's front-of-API) design

**Status: 🟠 four of five client pieces shipped; the phase is 🟠 only because two
of its three sidecars have no reference implementation and two server slices are
unstarted.**

| Client piece | Server | Client |
|---|---|---|
| Ask your library — retrieval and citations | ✅ | ✅ |
| Generated answers — `POST /chat` | ✅ | ✅ |
| People / faces browser, name-a-face, merge, reassign | ✅ | ✅ |
| "Similar files" — `GET /nodes/{id}/similar` | ✅ | ✅ |
| Feedback controls that feed labels back | ❌ | ❌ |

This document was first written when only retrieval could run, because the server
half did not exist. Both halves now do, and they met at the shapes the contract
specified with no drift — which is the seam working exactly as
[roadmap-split.md](roadmap-split.md) predicted.

What keeps the phase short of ✅ is behind the API, not here: ❌ streaming answers
(slice 4) and ❌ image-embedding similarity for photos (slice 5) are unstarted, and
❌ neither the **generation** sidecar (`POST /generate`) nor the **detection**
sidecar (`POST /detect`) has a reference image in `deploy/` — only the embedder
does. On a stock deployment, therefore, Ask returns citations without prose and the
people browser is correctly empty. Both are deliberate; see
[deferred-work.md](deferred-work.md).

Marks: ✅ done · 🟠 partial · ❌ not built; the ledger is [status.md](status.md).

## Ask your library ([web/src/Ask.tsx](../web/src/Ask.tsx)) ✅

A natural-language question answered by the documents most related to it, ranked
by meaning. It is the **retrieval** half of "chat over your documents", and it
runs entirely on the semantic search the server already shipped in Phase 4
(`GET /search?semantic=true`) — no new endpoint required.

- Type a question; the view returns the top related documents with a relevance
  bar (cosine similarity), content/folder match badges, and a link to open each.
- If the embedding sidecar is disabled the endpoint answers `503`; the view falls
  back to keyword search and says so, exactly as the file browser does.
- Surfacing the **source documents** first is deliberate: it is the trustworthy
  half of RAG, and it is useful on its own. That ordering is why this view could
  ship a phase before the generator existed, and why it still works on a server
  with no generator today.

**Since superseded, in the good way.** The view now calls `POST /chat`, which
returns those same passages as mandatory citations *plus* an answer when a
generator is configured — so the section below describes what replaced this one.
The fallback described here remains the behaviour when generation is off.

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

## ❌ Still not built

- ❌ **Feedback controls that feed labels back.** Unbuilt on both sides, and the
  only piece of this phase's original plan that nothing has started. It is also the
  one that needs a decision first: a correction to a face cluster is already
  permanent (`faces.dismissed_at`), so "feedback" here would mean labels that
  retrain a model, which Phase 4 put out of scope.
- ❌ **Streaming answers.** `stream: true` is in the contract and unimplemented.
  Citations are computed from the retrieved passages and are mandatory, so an answer
  that streamed ahead of its citations would be — for the duration of the stream —
  exactly the unverifiable output this design refuses to ship.
- ❌ **Image-embedding similarity.** "Find similar" works through the text
  embedding space, so it relates *documents*. Two photographs with no text have no
  neighbours; that needs a fourth model and a second vector space.
