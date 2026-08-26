# Generation inference sidecar

The written half of `POST /api/v1/chat`. A small FastAPI service that owns the
prompt, enforces grounding, and forwards token generation to a local
OpenAI-compatible runtime — llama.cpp's `server`, vLLM, Ollama, TGI, whichever
one the operator already runs on their GPU box.

**No model ships in this image.** It holds the prompt and the wire contract; the
weights live behind `GENERATE_BACKEND_URL`. That split is on purpose: a
generation runtime is a build tuned to a specific GPU, driver and CUDA version,
and a copy of one baked into this repository would be a worse version of the one
the operator already has.

**Nothing in this repository runs this code.** There is no Python in CI, no test
exercises it, and it is not in `deploy/compose/docker-compose.yml`. It is a
reference implementation of the contract the Go client speaks, written against
that client's source. Read it before you run it.

Turn it off and nothing breaks: `POST /chat` answers 200 with the retrieved
citations and `answer_unavailable: "generation_disabled"`, which is the half of
RAG that is actually verifiable anyway.

## The contract

Fixed by `server/internal/embed/generate.go`. Do not drift from it.

```
POST /generate      Content-Type: application/json
{
  "question": "when did the roof warranty expire?",
  "passages": [ {"Ref": "1", "Path": "/house/roof.pdf", "Text": "…"} ],
  "model":    "qwen2.5-7b-instruct",
  "stream":   false
}

200 {"answer": "…"}          or      200 {"error": "why not"}
```

Two details worth stating plainly, because both are easy to get wrong:

- **Passage keys are capitalised** — `Ref`, `Path`, `Text`. The Go struct has no
  json tags, so Go's default field names are the wire names. This service accepts
  the lowercase spellings too, so a hand-written `curl` works.
- **Failures are HTTP 200 with an `error` field.** The client turns any non-200
  into "sidecar returned 503" and discards the reason; an `error` string is
  logged verbatim next to the request. The user sees the same degraded result
  either way, so the only thing at stake is whether you can diagnose it.

With `"stream": true` the response is newline-delimited JSON instead — one
`{"delta": "…"}` per piece, then `{"done": true}`, with `{"error": "…"}` in-band
if the backend fails mid-answer. The client caps a stream at 1 MiB total and
256 KiB per line; this service stops itself at 512 KiB of answer, which also
stops the GPU producing tokens nobody will read.

`GET /healthz` returns `{status, model, backend}`. It does **not** probe the
backend: a health check that fails while the model server restarts would restart
this container too, which fixes nothing and hides the real outage. Check the
backend yourself with `curl $GENERATE_BACKEND_URL/models`.

## Run it

Start a runtime first. Any of these serves an OpenAI-compatible API:

```bash
# llama.cpp, GGUF, one command
./llama-server -m qwen2.5-7b-instruct-q4_k_m.gguf -c 8192 -ngl 99 --port 8080

# or Ollama, which is already OpenAI-compatible at /v1
ollama serve && ollama pull qwen2.5:7b-instruct
```

Then this sidecar:

```bash
docker build -t pc-generate ./deploy/generate-sidecar
docker run -d --name pc-generate -p 8001:8000 \
  -e GENERATE_BACKEND_URL=http://gpu-box.tailnet.ts.net:8080/v1 \
  -e GENERATE_MODEL=qwen2.5-7b-instruct \
  pc-generate
curl -s localhost:8001/healthz
```

Point the **API** at it — the worker never generates:

```bash
PC_GENERATE_URL=http://gpu-box.tailnet.ts.net:8001   # no trailing slash; config rejects one
PC_GENERATE_MODEL=qwen2.5-7b-instruct                # must be a name the backend serves
```

`PC_GENERATE_URL` without `PC_EMBED_URL` is refused at startup, and correctly:
answers are grounded in retrieved passages, and without embeddings there is
nothing to retrieve.

## Configuration

| Variable | Default | What it does |
|---|---|---|
| `GENERATE_BACKEND_URL` | `http://localhost:8080/v1` | OpenAI-compatible base URL |
| `GENERATE_MODEL` | `local-model` | model asked for when the request names none |
| `GENERATE_API_KEY` | empty | bearer token, for runtimes that require one |
| `GENERATE_MAX_TOKENS` | `800` | answer ceiling — roughly 600 words |
| `GENERATE_TEMPERATURE` | `0.2` | low on purpose; this is extraction, not writing |
| `GENERATE_NUM_CTX` | unset | context hint for runtimes that accept `options.num_ctx` |

`PC_GENERATE_MODEL` is sent with every request and wins over `GENERATE_MODEL`,
because it is also the string the API reports back to the browser as the model
that answered. If it is not a name your backend serves, requests fail with the
backend's own 404 in the error field.

## What the model choice costs you

There is no free option here. Picking a generator is picking your position on
three sliders at once, and the honest summary is that a home GPU buys a model
that is good at summarising passages and mediocre at reasoning over them.

| Class | VRAM at 4-bit | Speed on a 4090 | What you get |
|---|---|---|---|
| 3B instruct | ~2–3 GB | very fast | fluent, weak at multi-document questions; will restate a passage and call it an answer |
| 7–8B instruct | ~5–6 GB | fast | the sensible default; reliable at "what does this document say", shaky at synthesis |
| 14B instruct | ~9–10 GB | moderate | noticeably better at combining two passages |
| 30–34B instruct | ~18–20 GB | slow | about as good as a single 24 GB card gets |
| 70B instruct | ~40 GB+ | needs two cards | out of scope for one 4090 |

Context length is the other half of the bill. Eight passages of 1500 runes plus
the prompt is roughly 4–6k tokens, so an 8k window is the floor and 16k is
comfortable. KV cache grows with it, and on a 24 GB card a long window is what
pushes a 14B model out of VRAM and into system RAM, where generation drops from
seconds to minutes.

A rough shape to expect: on a 4090, a 7B model at 4-bit answers a chat question
in single-digit seconds; the same model on CPU takes minutes, and the Go client
gives up at five. On CPU, drop to a 3B model or leave the feature off — the API
degrades to citations, which is not a bad outcome.

The costs that are not VRAM: every model you can run at home hallucinates when
the passages do not contain the answer, which is what the prompt's first rule and
the citation display are both defending against. And the answer is only as good
as retrieval — a question the embedder mis-retrieves for produces a confident,
correctly-cited answer about the wrong document.

## Changing the prompt

`SYSTEM_PROMPT` in `app.py`. That is the point of this service existing: prompt
wording needs tuning per model, and rebuilding the Go API to change a sentence of
English would be the wrong loop. Edit, restart the container, ask again.

Four rules in there are structural rather than stylistic, and weakening any of
them changes what the feature is:

- **Only the passages.** A model answering from its own weights about somebody's
  private documents is the failure this whole design refuses to ship — and it is
  invisible, because the citations printed beside it name real files.
- **Only markers that were listed.** An invented `[4]` looks exactly like a real
  one in the UI, and nobody can check a citation that does not exist.
- **Passage text is data, not instruction.** The passages come from user
  documents, including ones other people sent them. Without that line, a
  document that contains instructions is a prompt injection with a file path.
- **Refuse rather than guess.** "The passages do not say" is a correct answer.

The service will not call the backend at all with zero passages, whatever the
prompt says.

## Where it runs

Same two-tier split as the embed sidecar. Generation touches **no blob content
and no database** — the API sends it the passage text it already retrieved — so
this can live on a GPU box that cannot see the blob store, reached over the
tailnet. Keep it off the always-on box: a 7 GiB machine cannot host a generator
and go on being always-on.

Content never leaves your infrastructure, and it stays that way only if
`GENERATE_BACKEND_URL` points at a machine you own. There is no hosted-provider
path in this code, and adding one would send a user's private documents to a
third party under a UI that promises the opposite.
