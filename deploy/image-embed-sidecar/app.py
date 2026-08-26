"""Image-embedding inference sidecar for Private Cloud photo similarity.

Loads a CLIP-style vision encoder — on a GPU when one is present — and answers
/embed-image with one vector per image. Those vectors are the SECOND vector
space: `/nodes/{id}/similar` ranks a photograph against other photographs in it,
where before slice 5 a picture with no extractable text had no neighbours at all.

Deliberately separate from the text embed sidecar for the reason the detection
one is: the models are different sizes with different resource profiles, and a
deployment may reasonably want semantic document search and no vision model.
Nothing else in this repository depends on this service running.

Endpoints:
    GET  /healthz      -> {status, model, dim, device}
    POST /embed-image  -> {"model": ..., "dim": N, "vector": [...]}
                          or {"error": "..."}
                          body: the raw image bytes, Content-Type is the MIME

One vector per image, not several: a document is embedded as passages because a
query can be about one paragraph of it, and an image is one thing.

Config (environment):
    IMAGE_EMBED_MODEL     identity reported at /healthz (default clip-vit-base-patch32)
    IMAGE_EMBED_HF_MODEL  the HuggingFace id actually loaded
    IMAGE_EMBED_MAX_SIDE  longest side the encoder runs at (default 1024)

The Go side stores every vector under PC_IMAGE_EMBED_MODEL and refuses any vector
that is not PC_IMAGE_EMBED_DIM wide, so those must match what this reports. A
width mismatch is the quiet failure: the ranking filter excludes every vector of
the wrong width, which looks exactly like a library nobody ever indexed. The wire
shape is fixed by server/internal/embed/imageclient.go.
"""

import io
import logging
import os

import numpy as np
import torch
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from PIL import Image
from transformers import CLIPModel, CLIPProcessor

# The identity, and the weights. Two variables rather than one because the Go
# side's PC_IMAGE_EMBED_MODEL is an operator-chosen name for a SPACE, not a
# HuggingFace id — the same split the text sidecar makes.
MODEL_NAME = os.environ.get("IMAGE_EMBED_MODEL", "clip-vit-base-patch32")
HF_MODEL = os.environ.get("IMAGE_EMBED_HF_MODEL", "openai/clip-vit-base-patch32")
MAX_SIDE = int(os.environ.get("IMAGE_EMBED_MAX_SIDE", "1024"))

# Mirrors embed.MaxImageBytes on the Go side. The worker will not send more than
# this, so a larger body is a different caller and gets refused rather than
# decoded.
MAX_INPUT_BYTES = 40 << 20

# Mirrors media.MaxPixels: the decompression-bomb guard. A 100-megapixel PNG is a
# few hundred kilobytes of compressible black and 400 MB of decoded RGBA, and
# this process is expected to be sharing a box rather than owning it.
MAX_PIXELS = 80_000_000
Image.MAX_IMAGE_PIXELS = MAX_PIXELS

DEVICE = "cuda" if torch.cuda.is_available() else "cpu"

log = logging.getLogger("image-embed")
logging.basicConfig(level=logging.INFO)

# Inference only, forever. Autograd on a forward-only service is wasted memory on
# the one resource this tier is short of.
torch.set_grad_enabled(False)

model = CLIPModel.from_pretrained(HF_MODEL).eval().to(DEVICE)
processor = CLIPProcessor.from_pretrained(HF_MODEL)

# Derived from the model rather than hardcoded to 512, so swapping the weights
# cannot leave this reporting a width it no longer serves.
DIM = int(model.config.projection_dim)

app = FastAPI(title="private-cloud image-embed sidecar")


def failure(message: str) -> JSONResponse:
    """An error the Go client can read.

    HTTP 200 with an error field rather than a status code, exactly as the
    detection sidecar does it: the client flattens any non-200 into "sidecar
    returned N" and discards the reason, while this string is logged verbatim
    beside the job that failed.
    """
    return JSONResponse(status_code=200, content={"error": message})


@app.get("/healthz")
def healthz():
    return {"status": "ok", "model": MODEL_NAME, "dim": DIM, "device": DEVICE}


@app.post("/embed-image")
async def embed_image(request: Request):
    mime = request.headers.get("content-type", "")
    data = await request.body()

    if not data:
        return failure("empty body")
    if len(data) > MAX_INPUT_BYTES:
        return failure("image is larger than " + str(MAX_INPUT_BYTES) + " bytes")

    try:
        img = Image.open(io.BytesIO(data))
        img.load()
    except Exception as e:
        # Pillow raises a wide family of errors for damaged files and for formats
        # it does not know, and every one of them means the same thing here.
        #
        # An ERROR rather than an empty vector, unlike the detector's "no faces":
        # there is no such thing as an image with no embedding, so there is no
        # honest empty answer to give. The Go handler returns it to the queue,
        # which retries with backoff and then dead-letters — and a file that is
        # not a decodable image will still not be one on the fifth attempt, so
        # this line is where the fact belongs.
        log.warning("undecodable image (content-type %s): %s", mime, e)
        return failure("undecodable image: " + str(e))

    if img.width * img.height > MAX_PIXELS:
        return failure("image exceeds the pixel bound")

    # RGB because the encoder wants three channels, and a greyscale scan or a
    # palettised PNG is otherwise a shape error rather than a picture.
    img = img.convert("RGB")

    # Downscaled before the processor's own resize. CLIP's preprocessor crops to
    # 224px anyway, so this changes nothing about the vector — it bounds the
    # PIL-side resample of a 50-megapixel original, which is the part that costs
    # seconds rather than milliseconds.
    scale = min(1.0, MAX_SIDE / max(img.width, img.height))
    if scale < 1.0:
        img = img.resize((max(int(img.width * scale), 1), max(int(img.height * scale), 1)))

    try:
        inputs = processor(images=img, return_tensors="pt").to(DEVICE)
        features = model.get_image_features(**inputs)
    except Exception as e:
        # A real failure — out of memory, a broken CUDA context. Worth a retry.
        log.exception("encoder failed")
        return failure("encoder failed: " + str(e))

    vector = features[0].cpu().numpy().astype(np.float32)
    norm = float(np.linalg.norm(vector))
    if norm == 0:
        # A zero vector has no direction, so its cosine to everything is zero and
        # it would sit at the bottom of every ranking forever. Storing it would
        # also make the photo look indexed, which is worse than it looking
        # unindexed: the handler's idempotence check would then never retry it.
        return failure("encoder produced a zero vector")
    unit = vector / norm

    log.info("embedded image (%s, dim %d)", mime, DIM)
    return {
        "model": MODEL_NAME,
        "dim": DIM,
        # Rounded because float32 carries about seven significant digits, so this
        # is lossless where it lands and roughly halves the response body.
        "vector": [round(float(v), 6) for v in unit],
    }
