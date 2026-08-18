# Phase 8 — Advanced intelligence (behind the API) design

**Status: 🟠 3/5 slices ✅ complete; slices 4 and 5 ❌ not started; and none of the
three shipped surfaces has a client yet.** `/chat`, `/similar`, `/people` and both
face routes are declared in `awaitingClient` — built, tested, unreached. Two of the
three sidecars this phase depends on also have ❌ no reference implementation in
`deploy/`: only the embedder does. Marks: ✅ done · 🟠 partial · ❌ not built; the
ledger is [status.md](status.md).

The server half of the phase whose client half is
[phase-8-design.md](phase-8-design.md) — where "Ask your library" already shipped
as the *retrieval* view, because retrieval was the one piece that ran on what
Phase 4 had built. This adds the generation, similarity and faces behind it.

**Exit criterion:** a person can ask a question of their own documents and get an
answer that says which documents it came from; find files like a file they are
looking at; and browse their photos by who is in them, correcting the machine
when it is wrong.

**The rule the whole phase obeys:** every endpoint here depends on a sidecar, and
every one degrades with a stable code rather than a 500 or a hang. The file API,
the gallery, sync and search all stay fully functional with all of it switched
off. That is the property the two-tier split exists to protect, and it is the
reason none of this lives in the API process.

---

## 0. Three sidecars, not one

| Sidecar | Env | Used by | Client in Go | Reference image in `deploy/` |
|---|---|---|---|---|
| Embedding | `PC_EMBED_URL` | semantic search, similarity, chat retrieval | ✅ | ✅ `deploy/embed-sidecar` |
| Generation | `PC_GENERATE_URL`, `PC_GENERATE_MODEL` | `POST /chat` written answers | ✅ | ❌ nothing serves `POST /generate` |
| Detection | `PC_DETECT_URL`, `PC_DETECT_MODEL`, `PC_DETECT_DIM` | the `faces` job | ✅ | ❌ nothing serves `POST /detect` |

The two ❌s are deliberate and are recorded in [deferred-work.md](deferred-work.md):
an embedder is small and interchangeable, so shipping one reference image was worth
it; a generator is a multi-gigabyte decision about quality, latency and VRAM, and a
face detector is a decision about which biometric model an operator is willing to
run. The consequence to know is that on a stock deployment `POST /chat` answers
with citations and no prose, and the `faces` job does nothing at all.

Separate because they have very different resource profiles and a deployment may
reasonably want one and not the others. An embedder is a single forward pass and
shares a modest box happily; a generator holds a much larger model; a face
detector is a third model again. Folding detection into the media job would have
tied thumbnailing — which every deployment wants — to a sidecar most will not
run.

Config rejects `PC_GENERATE_URL` without `PC_EMBED_URL`: a generator with nothing
to retrieve would answer every question from nothing at all.

**Content never leaves your infrastructure.** These are services you run. There
is no hosted-provider path here, and adding one would break the promise the
project is built on.

## 1. Similarity and retrieval are one mechanism

"Find documents like this one" and "find documents related to this question" are
the same operation with a different source vector. They share one scan, and there
is exactly **one place where the ACL filter meets the vector store**. Building
them separately would mean two nearly identical scans that could drift apart in
what they consider visible — and for an ACL filter, drift means a leak.

That filter is on the **node** rows, never the vectors. Embeddings are
content-addressed, so two users owning the same document share one vector row by
construction; filtering vectors would either hide a document from someone
entitled to it or let one user's query surface another's document through a
similarity score.

**Reading the source is required** for `/similar`. Without that check any node id
becomes a probe: the scores it produces leak the shape of a private document
without ever returning a byte of it.

A file is excluded from its own similarity results — returning it first at 1.0 is
correct and useless. A document is similar if **any** of its passages is close to
**any** of the source's, not on an average, because averaging lets one long
unrelated section drown a genuine match.

An unindexed file is `404 not_indexed`, not an empty list and not a `503`. Empty
would claim "nothing resembles this"; `503` would tell the client to retry a
feature that is working fine.

**Passage text is not stored beside the vector.** `doc_text` already holds the
full text content-addressed and `ChunkText` is deterministic, so a passage is
exactly `ChunkText(text)[seq]`. Duplicating it would double storage for every
document and add a second copy that could drift from the first when the chunker
changes. It is recovered after ranking, for the handful of chunks that survived,
one `doc_text` read per distinct document.

## 2. Chat: two halves with different reliability

Retrieval runs wherever semantic search runs. Generation needs a GPU box and is
optional. **With no generator, `POST /chat` still answers `200`** with the
passages and `answer_unavailable: "generation_disabled"`.

That is deliberate, not a fallback. Surfacing the source documents is the
trustworthy half of RAG and useful alone — it is exactly what the shipped web
view renders. Refusing the whole request because an optional component is absent
would take away a working feature to punish the absence of one that never
existed. A generator that *fails* degrades the same way.

**Citations are mandatory, not decorative.** An answer over someone's own
documents that cannot say which document it came from is unverifiable, and a
confident wrong answer about your own files is worse than no feature. They are
present whether or not an answer was generated, and the generator is handed
passage **text** — an answer grounded in filenames alone would be invention.

**Nothing retrieved means no answer.** The generator is not called at all;
`answer_unavailable: "no_matching_documents"`. Answering anyway is the model
inventing something, which is the one failure this design refuses to ship.

**Retrieval reaches only what the caller could already open**, so chat cannot
become a way to read around a permission. Under Phase 7 the ACL applies to
retrieval for the same reason it applies to search.

The prompt lives in the sidecar, not in Go. Prompt wording is the part most
likely to need tuning against a specific model, and rebuilding the API to change
a sentence of English is the wrong iteration loop.

`scope.node_ids` and `scope.tags` from the contract are **not accepted**. A scope
field that parses and silently does nothing is worse than an absent one, because
the caller believes their question was narrowed when it was not. Only
`scope.under` works.

## 3. Faces: per-owner, and correctable by design

**Not content-addressed**, unlike document embeddings, and the difference is the
whole point. A document's vector describes its bytes and may safely be shared
between two users owning the same file. A "people" graph describes who someone
knows: two users owning the same photograph must not share clusters, or naming a
face in your library would name it in a stranger's.

**Clustering is incremental and greedy.** Each new face joins the nearest cluster
above `FaceMatchThreshold` (0.72) or starts one of its own. Not a global
re-partition, for an operational reason rather than a mathematical one: a library
grows one upload at a time, and re-partitioning on each arrival would either run
constantly or keep renaming clusters a person has already named. **A name is a
promise; re-clustering must not silently break it.**

The centroid moves toward each face added, so a cluster tracks a person across
lighting and age rather than being pinned to whichever photo arrived first.

The threshold errs toward **splitting**. Too low merges two people, which a user
experiences as the feature being wrong about who someone is; too high scatters
one person across clusters, which they experience as it being incomplete. The
second is one click to fix.

Which is why **merge and reassign are part of the design, not an afterthought**.
Greedy assignment depends on arrival order, so clustering *will* be wrong — and a
faces feature with no correction path is one people stop trusting after the first
mistake.

- A cluster is **unnamed until a person names it**. The system never guesses an
  identity: an unnamed cluster is an honest "these faces look alike", a guessed
  name is a claim about a real human being that nobody made.
- **Forgetting a cluster keeps the detections** (`ON DELETE SET NULL`, not
  `CASCADE`). The faces are still in the photographs; re-running detection to
  recover them would be pure waste.
- **Reassigning to nothing** detaches a face without deleting it — how a user
  says "that is not a face" without discarding the detection.
- Bounding boxes are **fractions, not pixels**, so a client crops from whichever
  variant it already holds.
- A photo with **no** faces is still recorded as looked-at, or every faceless
  photo is re-detected on every reindex forever.

`cloudctl jobs reindex --kind=faces` backfills, and is deliberately **not** part
of `--kind=all`: queueing a job per photo on a server with no detector would fill
the dead-letter queue rather than do anything useful.

## 4. Slices

| # | Slice | Status |
|---|---|---|
| **1** | Similar files + the shared retrieval layer | ✅ 8 tests — ❌ no client |
| **2** | `POST /chat`, generation sidecar client, degraded modes | ✅ 7 tests — ❌ no client, and ❌ no generation sidecar image |
| **3** | Face schema, detector client, `faces` job, clustering, `/people` | ✅ 8 tests — ❌ no client, and ❌ no detection sidecar image |
| 4 | Streaming answers (`stream: true`, SSE) | ❌ the contract specifies it; non-streaming ships first because a written answer that arrives whole is strictly simpler to render and to test |
| 5 | Image-embedding similarity for photos | ❌ `/similar` covers documents; photos need an image embedder, a fourth model |

## 5. Risks

- **The clustering scan is bounded but not indexed.** `MaxFaceScan` keeps one
  pass finite, but assignment is O(faces × clusters) in the application. At a few
  thousand faces this is milliseconds; a library with a hundred thousand wants
  the same pgvector upgrade the document path is already written to accept.
- **Greedy clustering is order-dependent**, so two libraries with the same photos
  in a different upload order can produce different clusters. Correctable, and
  the correction is permanent, but it means clustering is not reproducible.
- **A generated answer is only as good as its retrieval.** Citations make a wrong
  answer *checkable*, which is the strongest guarantee available here — it is not
  a guarantee that the answer is right, and the UI should not imply that it is.
- **Face detection is the most privacy-sensitive thing this server does.** It is
  off unless a detector is configured, per-owner, and never shared — but an
  operator enabling it should know they are building a biometric index of
  everyone who has ever been photographed by the people using their server.
