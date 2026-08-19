# No model, no CUDA, no torch. This container owns the prompt and the wire shape;
# the weights live in whatever OpenAI-compatible runtime GENERATE_BACKEND_URL
# points at. That is why this image is tens of megabytes where the embed sidecar
# is gigabytes, and why it starts instantly rather than after a model load.
FROM python:3.12-slim

WORKDIR /app

# curl is for the healthcheck below; there is nothing here that needs compiling.
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .

# localhost is a deliberate default that fails loudly rather than a hostname that
# might resolve to something unintended. Set it to the runtime's address — over
# the tailnet or the compose network — when running this anywhere real.
ENV GENERATE_BACKEND_URL=http://localhost:8080/v1
ENV GENERATE_MODEL=local-model

EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -fsS http://localhost:8000/healthz || exit 1

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
