# Phase 8 — Advanced intelligence (Vivian's front-of-API) design

**Status: 🟠 one of four client pieces shipped. The server behind the other three
is now ✅ complete, and this is the widest gap in the project.**

| Client piece | Server | Client |
|---|---|---|
| Ask your library — retrieval, via `/search?semantic=true` | ✅ | ✅ |
| Generated answers with citations — `POST /chat` | ✅ | ❌ |
| People / faces browser, name-a-face, merge, reassign | ✅ | ❌ |
| "Similar files" — `GET /nodes/{id}/similar` | ✅ | ❌ |
| Feedback controls that feed labels back | ❌ | ❌ |

When this document was written, "needs new server surface" was the accurate reason
for the last three. It is not any more —
[phase-8-server-design.md](phase-8-server-design.md) shipped all of it, with
mandatory citations and stable degraded codes. The reason those three views do not
exist is now simply that they have not been built.

Marks: ✅ done · 🟠 partial · ❌ not built; the ledger is [status.md](status.md).

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
  4090), which **now exists**: `POST /chat` returns those same passages as
  mandatory citations, plus an answer when a generator is configured.

## ❌ Not yet — and no longer waiting on the server

- ❌ **Generated answers.** `POST /chat` is built, tested and served, and it
  returns `200` with citations even when no generator is configured
  (`answer_unavailable: "generation_disabled"`) — which is *exactly* the shape this
  view already renders. Switching Ask from `/search?semantic=true` to `/chat` would
  lose nothing and gain the answer whenever a generator is present. It is declared
  in `awaitingClient` as "the Ask view still calls `/search?semantic=true`".
- ❌ **People / faces.** `GET /people`, `GET /people/{id}`, `PATCH /people/{id}`
  (name it), `POST /people/{id}/merge`, `GET /nodes/{id}/faces` and
  `POST /nodes/{id}/faces/{faceId}/reassign` are all served. Bounding boxes are
  fractions, so a client crops from whichever variant it already holds. Clusters
  arrive **unnamed** — the server never guesses an identity — so the naming UI is
  not a nicety, it is the thing that turns a cluster into a person.
- ❌ **Similar files.** `GET /nodes/{id}/similar` is served; an unindexed file
  answers `404 not_indexed` rather than an empty list, so the affordance can say
  "not indexed yet" instead of "nothing resembles this".
- ❌ **Feedback controls.** Still genuinely unbuilt on both sides.

Two of these also need an operator to stand up a sidecar this repository does not
ship — ❌ the generation sidecar (`POST /generate`) and ❌ the detection sidecar
(`POST /detect`) are a Go client and a config variable each. A people browser
against a server with no detector correctly shows no people; see
[deferred-work.md](deferred-work.md).
