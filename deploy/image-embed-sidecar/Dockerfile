# CPU-default image, like the other three sidecars: the model loads on CPU here
# and on a GPU automatically when the container is run with `--gpus all`.
#
# Large, because transformers pulls torch — the same honest cost the detection
# sidecar carries. If the image size is the objection, the answer is not to build
# it: nothing in this repository needs it, and `/similar` keeps ranking in the
# document space with the image space simply absent.
FROM python:3.12-slim

WORKDIR /app

# Build tooling for any wheels that need compiling, removed after install to keep
# the image lean.
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential curl \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .

# The weights, and the identity vectors are stored under. Baked in so the first
# photo the worker sends does not block on a multi-hundred-megabyte download
# while its job times out — on a machine that may have no route to the internet
# at all.
#
# IMAGE_EMBED_MODEL must equal PC_IMAGE_EMBED_MODEL on the Go side, and
# PC_IMAGE_EMBED_DIM must equal the `dim` this reports at /healthz, or every
# vector is refused on arrival and no photo ever ranks — which looks exactly like
# a library nobody indexed.
ENV IMAGE_EMBED_HF_MODEL=openai/clip-vit-base-patch32
ENV IMAGE_EMBED_MODEL=clip-vit-base-patch32
RUN python -c "import os; from transformers import CLIPModel, CLIPProcessor; m=os.environ['IMAGE_EMBED_HF_MODEL']; CLIPModel.from_pretrained(m); CLIPProcessor.from_pretrained(m)"

EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -fsS http://localhost:8000/healthz || exit 1

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
