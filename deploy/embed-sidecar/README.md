# Embedding inference sidecar

The model tier for semantic search. A small FastAPI service that loads a
sentence-embedding model and answers `/embed`. It runs **separately** from the Go
API and worker — on a GPU box (your 4090s) reached over the tailnet, or on the
always-on box as a slow CPU fallback — so no model is ever resident in the API's
memory budget.

This is the piece the Phase 4 design calls the "GPU worker tier": the Go worker
enqueues embedding jobs, calls this sidecar to turn document text into vectors,
and stores them; the Go API calls it to embed a query at search time. Turn the
sidecar off and semantic search reports itself unavailable while keyword and OCR
search keep working.

## Run it

**On a GPU box** (recommended — this is what the 4090s are for):

```bash
docker build -t pc-embed ./deploy/embed-sidecar
docker run -d --name pc-embed --gpus all -p 8000:8000 pc-embed
curl -s localhost:8000/healthz     # {"status":"ok","model":"...","dim":384,"device":"cuda"}
```

`--gpus all` needs the NVIDIA Container Toolkit on the host. The base image is
CPU Python; PyTorch uses the GPU at runtime when it is visible. For a
CUDA-optimized build, change the base image to an `nvidia/cuda:*-runtime` image
and install a matching torch wheel — the app code is unchanged.

**CPU fallback** (works anywhere, slower): the same image with no `--gpus` flag
loads the model on CPU.

## Wire it to the API and worker

Point both processes at the sidecar and tell them what it serves. These MUST
match the model — the Go side stores vectors under `PC_EMBED_MODEL` and validates
their width against `PC_EMBED_DIM`:

```bash
PC_EMBED_URL=http://gpu-box.tailnet.ts.net:8000
PC_EMBED_MODEL=bge-small-en-v1.5     # any stable identity string
PC_EMBED_DIM=384                     # must equal the model's dimension
```

- On the **worker** (`pcworker`), these register the embed handler and chain
  extraction to enqueue embedding jobs.
- On the **API**, they enable the semantic endpoint (`GET /api/v1/search?semantic=true`),
  which embeds the query through this sidecar and ranks by cosine similarity.

## Choosing a model

`BAAI/bge-small-en-v1.5` (384-dim) is a strong small default. On a 4090 you can
afford a larger model (`bge-base`/`bge-large`, `e5-large-v2`) for better recall —
set `EMBED_MODEL`, update `PC_EMBED_DIM` to its dimension, and re-index (the
worker re-embeds under the new model identity; old vectors are pruned as content
is rewritten, or drop the `doc_embedding` rows to force a clean re-embed).

Vectors are L2-normalized here, so the Go side's cosine similarity is a dot
product. Content never leaves your tailnet: this is local inference on your own
hardware, which is the whole point.
