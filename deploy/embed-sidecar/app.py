"""Embedding inference sidecar for Private Cloud semantic search.

A small HTTP service that loads a sentence-embedding model — on a GPU when one is
present — and answers /embed. It is deliberately separate from the Go API and
worker: the model lives here, so the always-on box never holds one resident, and
this can run on a GPU box (the 4090s) that the worker reaches over the tailnet.

Endpoints:
    GET  /healthz -> {status, model, dim, device}
    POST /embed   -> {model, dim, vectors}   body: {"texts": ["...", ...]}

Config (environment):
    EMBED_MODEL   HuggingFace model id (default BAAI/bge-small-en-v1.5, dim 384)

The Go side stores vectors under the identity in PC_EMBED_MODEL and validates the
dimension against PC_EMBED_DIM, so those must match this model. Vectors are
L2-normalized here, so cosine similarity on the Go side is a plain dot product.
"""

import os

import torch
from fastapi import FastAPI
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer

MODEL_NAME = os.environ.get("EMBED_MODEL", "BAAI/bge-small-en-v1.5")
DEVICE = "cuda" if torch.cuda.is_available() else "cpu"

model = SentenceTransformer(MODEL_NAME, device=DEVICE)
DIM = model.get_sentence_embedding_dimension()

app = FastAPI(title="private-cloud embed sidecar")


class EmbedRequest(BaseModel):
    texts: list[str]


@app.get("/healthz")
def healthz():
    return {"status": "ok", "model": MODEL_NAME, "dim": DIM, "device": DEVICE}


@app.post("/embed")
def embed(req: EmbedRequest):
    # normalize_embeddings=True so the Go side's cosine is a dot product, and a
    # zero-length input list returns an empty result rather than erroring.
    vectors = model.encode(
        req.texts,
        normalize_embeddings=True,
        batch_size=32,
        convert_to_numpy=True,
    )
    return {"model": MODEL_NAME, "dim": DIM, "vectors": [v.tolist() for v in vectors]}
