# CPU-default image, like the embed sidecar: the models load on CPU here and on a
# GPU automatically when the container is run with `--gpus all`. See the README
# for the CUDA variant.
#
# This one is large — facenet-pytorch pulls torch and torchvision — and that is
# the honest cost of running a face model. If the image size is the objection,
# the answer is not to build it: nothing in this repository needs it, and the
# faces job simply does not run.
FROM python:3.12-slim

WORKDIR /app

# Build tooling for any wheels that need compiling, removed after install to keep
# the image lean. libgl/libglib are what OpenCV — pulled in by facenet-pytorch —
# links against; without them the import fails at runtime rather than at build.
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential curl libgl1 libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .

# Bake both sets of weights into the image. facenet-pytorch downloads them to a
# cache on first use, and without this the first photo the worker sends would
# block on a download while its job timed out — on a machine that may have no
# route to the internet at all.
RUN python -c "from facenet_pytorch import MTCNN, InceptionResnetV1; MTCNN(); InceptionResnetV1(pretrained='vggface2')"

# The identity every face vector is stored under. It must equal PC_DETECT_MODEL
# on the Go side, and PC_DETECT_DIM must equal the `dim` this reports, or every
# detection is silently dropped — which looks exactly like a library with no
# faces in it.
ENV DETECT_MODEL=facenet

EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -fsS http://localhost:8000/healthz || exit 1

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
