# CPU-default image. The model loads on CPU here and on GPU automatically when the
# container is run with GPU access — see the README for the CUDA variant and the
# `--gpus all` runtime flag that lets this same image use a 4090.
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

# Bake the default model into the image so the first request does not block on a
# multi-hundred-megabyte download. Override EMBED_MODEL at build to pre-pull a
# different one.
ENV EMBED_MODEL=BAAI/bge-small-en-v1.5
RUN python -c "import os; from sentence_transformers import SentenceTransformer; SentenceTransformer(os.environ['EMBED_MODEL'])"

EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -fsS http://localhost:8000/healthz || exit 1

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
