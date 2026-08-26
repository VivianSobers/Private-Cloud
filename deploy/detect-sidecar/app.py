"""Face detection inference sidecar for Private Cloud photos.

Loads a face detector and a face encoder — on a GPU when one is present — and
answers /detect with one box and one vector per face. The Go worker clusters
those vectors into people; this service names nobody and knows nobody.

It is deliberately separate from the worker for the same reason the embed sidecar
is: the model lives here, so the always-on box never holds one resident, and this
can run on a GPU box reached over the tailnet.

Endpoints:
    GET  /healthz -> {status, model, dim, device}
    POST /detect  -> {"faces": [{"box": [x, y, w, h], "vector": [...]}]}
                     or {"error": "..."}
                     body: the raw image bytes, Content-Type is the image's MIME

Boxes are x, y, w, h as FRACTIONS of the image, never pixels: the web client
crops from whichever variant it already holds — original, large, thumbnail — and
a pixel box would only be correct for one of them.

Config (environment):
    DETECT_MODEL        identity string reported at /healthz (default facenet)
    DETECT_MIN_PROB     detector confidence floor (default 0.90)
    DETECT_MIN_FACE_PX  smallest face the detector looks for (default 40)
    DETECT_MAX_FACES    faces returned per image (default 50)
    DETECT_MAX_SIDE     longest side detection runs at (default 1600)

The Go side stores every vector under PC_DETECT_MODEL and drops any detection
whose vector is not PC_DETECT_DIM wide, so those must match this service. The
wire shape is fixed by server/internal/media/faces.go.

WHAT THIS BUILDS: running it turns a photo library into a biometric index of
everyone who appears in it, including people who never used this server and were
never asked. See the README before turning it on.
"""

import io
import logging
import os

import numpy as np
import torch
from facenet_pytorch import MTCNN, InceptionResnetV1
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from PIL import Image

MODEL_NAME = os.environ.get("DETECT_MODEL", "facenet")
MIN_PROB = float(os.environ.get("DETECT_MIN_PROB", "0.90"))
MIN_FACE_PX = int(os.environ.get("DETECT_MIN_FACE_PX", "40"))
MAX_FACES = int(os.environ.get("DETECT_MAX_FACES", "50"))
MAX_SIDE = int(os.environ.get("DETECT_MAX_SIDE", "1600"))

# Mirrors media.MaxInputBytes on the Go side. The worker will not send more than
# this, so a larger body is a different caller and gets refused rather than
# decoded.
MAX_INPUT_BYTES = 40 << 20

# Mirrors media.MaxPixels: the decompression-bomb guard. A 100-megapixel PNG is a
# few hundred kilobytes of compressible black and 400 MB of decoded RGBA, and
# this process is expected to be sharing a box with a GPU workload rather than
# owning it.
MAX_PIXELS = 80_000_000
Image.MAX_IMAGE_PIXELS = MAX_PIXELS

DEVICE = "cuda" if torch.cuda.is_available() else "cpu"

log = logging.getLogger("detect")
logging.basicConfig(level=logging.INFO)

# Inference only, forever. Autograd on a forward-only service is wasted memory on
# the one resource this tier is short of.
torch.set_grad_enabled(False)

# keep_all because a family photo is the point; select_largest=False so the
# ordering is the detector's, not "biggest face wins", and confidence decides
# what survives below.
detector = MTCNN(
    keep_all=True,
    select_largest=False,
    min_face_size=MIN_FACE_PX,
    post_process=True,
    device=DEVICE,
)
encoder = InceptionResnetV1(pretrained="vggface2").eval().to(DEVICE)

# Derived from the encoder rather than hardcoded to 512, so swapping the encoder
# cannot leave this reporting a width it no longer serves. PC_DETECT_DIM must
# equal what /healthz says here: the worker silently drops every detection of the
# wrong width, which looks exactly like a library with no faces in it.
DIM = int(encoder(torch.zeros(1, 3, 160, 160, device=DEVICE)).shape[1])

app = FastAPI(title="private-cloud detect sidecar")


def failure(message: str) -> JSONResponse:
    """An error the Go client can read.

    HTTP 200 with an error field rather than a status code, because the client
    turns any non-200 into "sidecar returned 503" and discards the reason, while
    this string is logged verbatim beside the job that failed.
    """
    return JSONResponse(status_code=200, content={"error": message})


def nothing_found(reason: str, mime: str) -> dict:
    """No faces, and no error — a completed job.

    Used for input this service cannot read at all. It is tempting to report that
    as a failure, but the worker retries every error with backoff and then
    dead-letters it, and a file that is not a decodable image will not become one
    on the fifth attempt. The log line is where that fact belongs.
    """
    log.warning("no detection possible: %s (content-type %s)", reason, mime)
    return {"faces": []}


@app.get("/healthz")
def healthz():
    return {"status": "ok", "model": MODEL_NAME, "dim": DIM, "device": DEVICE}


@app.post("/detect")
async def detect(request: Request):
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
        # Pillow raises a wide family of errors for damaged files, and every one
        # of them means the same thing here.
        return nothing_found("undecodable image: " + str(e), mime)

    if img.width * img.height > MAX_PIXELS:
        return nothing_found("image exceeds the pixel bound", mime)

    # RGB because the models want three channels, and a greyscale scan or a
    # palettised PNG is otherwise a shape error rather than a face.
    #
    # No EXIF transpose on purpose. Boxes are fractions of the image AS STORED,
    # which is the coordinate space the thumbnails and the web client both work
    # in — neither of them rotates either. Rotating here alone would put every
    # box on a sideways photo in the wrong place, which is worse than the
    # detector doing poorly on one.
    img = img.convert("RGB")

    # Detection runs at a bounded size. Fractions make this free: a box divided
    # by the dimensions it was measured in is the same number at any scale. The
    # cost is that DETECT_MIN_FACE_PX applies to the scaled image, so a face in
    # the back row of a 6000px group photo can fall under it — raise MAX_SIDE if
    # that matters more than the latency.
    scale = min(1.0, MAX_SIDE / max(img.width, img.height))
    if scale < 1.0:
        img = img.resize((int(img.width * scale), int(img.height * scale)))
    width, height = img.width, img.height
    if width == 0 or height == 0:
        return nothing_found("zero-dimension image", mime)

    try:
        boxes, probs = detector.detect(img)
    except Exception as e:
        # A real failure — out of memory, a broken CUDA context. Worth a retry,
        # so it goes back as an error rather than as an empty result.
        log.exception("detector failed")
        return failure("detector failed: " + str(e))

    if boxes is None or len(boxes) == 0:
        # The common case. Most photos have no faces, and the worker records that
        # so the photo is not re-detected on every reindex forever.
        return {"faces": []}

    # Confidence first, then a hard cap. Both bound the response: the Go client
    # reads at most 8 MiB, and one 512-float vector is several kilobytes of JSON.
    # The cap discards the least confident faces rather than an arbitrary tail.
    kept = [
        (box, float(prob))
        for box, prob in zip(boxes, probs)
        if prob is not None and float(prob) >= MIN_PROB
    ]
    kept.sort(key=lambda bp: bp[1], reverse=True)
    kept = kept[:MAX_FACES]
    if not kept:
        return {"faces": []}

    try:
        crops = detector.extract(img, np.array([box for box, _ in kept]), None)
        vectors = encoder(crops.to(DEVICE)).cpu().numpy()
    except Exception as e:
        log.exception("encoder failed")
        return failure("encoder failed: " + str(e))

    faces = []
    for (box, _prob), vector in zip(kept, vectors):
        # Clamped because a detector routinely puts part of a box past the edge
        # of the image on a face that is half out of frame, and the worker drops
        # any box that is not inside [0, 1].
        x1 = min(max(float(box[0]) / width, 0.0), 1.0)
        y1 = min(max(float(box[1]) / height, 0.0), 1.0)
        x2 = min(max(float(box[2]) / width, 0.0), 1.0)
        y2 = min(max(float(box[3]) / height, 0.0), 1.0)
        w, h = x2 - x1, y2 - y1
        if w <= 0 or h <= 0:
            # Nothing to crop and nothing to show. Dropped here rather than sent
            # for the worker to drop, so the count in its log is the truth.
            continue

        norm = float(np.linalg.norm(vector))
        if norm == 0:
            continue
        # L2-normalized so a person's centroid is a mean of comparable vectors.
        # The Go side's cosine normalizes anyway, but the running centroid it
        # keeps is a plain weighted mean, and one long vector in it would drag
        # the cluster around by magnitude rather than by resemblance.
        unit = vector / norm

        faces.append(
            {
                # Rounded because float32 carries about seven significant digits,
                # so this is lossless where it lands, and it roughly halves a
                # response that has an 8 MiB ceiling.
                "box": [round(x1, 6), round(y1, 6), round(w, 6), round(h, 6)],
                "vector": [round(float(v), 6) for v in unit],
            }
        )

    log.info("detected %d faces", len(faces))
    return {"faces": faces}
