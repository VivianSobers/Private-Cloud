package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"log/slog"
)

// Face detection (Phase 8).
//
// FaceKind is its own job kind rather than extra work inside the media job. The
// two need different machines: media analysis wants CPU and file bytes, face
// detection wants a model on a GPU. Folding detection into the media handler
// would tie thumbnailing — which every deployment wants — to a sidecar most
// deployments will not run.
const FaceKind = "faces"

// ErrDetectionUnavailable means no detector is configured or it could not be
// reached. Like every other Phase 8 dependency this degrades: the file API, the
// gallery and thumbnails are all unaffected by its absence.
var ErrDetectionUnavailable = errors.New("face detection is not available")

// Detection is one face found in an image.
type Detection struct {
	// Box is x, y, w, h as FRACTIONS of the image, so a client can crop from
	// whichever variant it already holds.
	Box [4]float64 `json:"box"`
	// Vector is the face embedding used to cluster this face with others.
	Vector []float32 `json:"vector"`
}

// Detector finds faces in image bytes. An interface so the handler can be tested
// without a GPU, exactly as the embedder is.
type Detector interface {
	Detect(ctx context.Context, mime string, data []byte) ([]Detection, error)
	Model() string
	Dim() int
}

// DetectClient is a Detector backed by an HTTP sidecar, the same shape as the
// embedding and generation clients: the model lives on a box with a GPU and this
// process holds none of it.
type DetectClient struct {
	base  string
	model string
	dim   int
	http  *http.Client
}

func NewDetectClient(base, model string, dim int) *DetectClient {
	return &DetectClient{
		base:  base,
		model: model,
		dim:   dim,
		// Longer than the embedder's 30s: detection decodes an image and runs a
		// model over it, and this is a background job rather than a synchronous
		// request path, so patience costs nothing a user is waiting on.
		http: &http.Client{Timeout: 2 * time.Minute},
	}
}

func (c *DetectClient) Model() string { return c.model }
func (c *DetectClient) Dim() int      { return c.dim }

type detectResponse struct {
	Faces []Detection `json:"faces"`
	Error string      `json:"error,omitempty"`
}

// Detect posts the image bytes to the sidecar and reads back its detections.
func (c *DetectClient) Detect(ctx context.Context, mime string, data []byte) ([]Detection, error) {
	if c.base == "" {
		return nil, ErrDetectionUnavailable
	}

	url := strings.TrimRight(c.base, "/") + "/detect"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mime)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDetectionUnavailable, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDetectionUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: sidecar returned %d", ErrDetectionUnavailable, resp.StatusCode)
	}

	var out detectResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: malformed response", ErrDetectionUnavailable)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrDetectionUnavailable, out.Error)
	}
	return out.Faces, nil
}

// FaceStore is what the face handler needs from the database.
type FaceStore interface {
	ReplaceFaces(ctx context.Context, ownerID, nodeID uuid.UUID, model string, dim int, faces []StoredFace) error
}

// StoredFace mirrors files.Face, declared here so this package does not import
// files — files imports media, and the dependency must not run both ways. The
// adapter converts, which is a few lines in one place rather than a cycle.
type StoredFace struct {
	Box    [4]float64
	Vector []float32
}

// FaceHandler detects and stores the faces in one image.
type FaceHandler struct {
	opener   Opener
	store    FaceStore
	detector Detector
	log      *slog.Logger
}

func NewFaceHandler(opener Opener, store FaceStore, detector Detector, log *slog.Logger) *FaceHandler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &FaceHandler{opener: opener, store: store, detector: detector, log: log}
}

// Handle runs detection over one node.
//
// Returns nil for every "nothing to do" outcome — not an image, content gone, no
// faces present — and a real error only for something worth retrying. A photo
// with no faces in it is a COMPLETED job, not a failed one: most photos have no
// faces, and treating that as failure would dead-letter most of a library.
func (h *FaceHandler) Handle(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID) error {
	if nodeID == nil {
		return nil
	}

	fc, err := h.opener.OpenForMedia(ctx, ownerID, *nodeID)
	if errors.Is(err, ErrContentGone) {
		return nil
	}
	if err != nil {
		return err
	}
	defer fc.Reader.Close()

	// Images only. Video face detection needs a frame sampler, which is the same
	// demuxer problem the media package already declines to solve in-process.
	if !isImage(fc.MIME) {
		return nil
	}

	data, err := io.ReadAll(io.LimitReader(fc.Reader, MaxInputBytes))
	if err != nil {
		return err
	}

	detections, err := h.detector.Detect(ctx, fc.MIME, data)
	if errors.Is(err, ErrDetectionUnavailable) {
		// Worth retrying: the sidecar may be restarting. Returned as an error so
		// the queue's backoff applies rather than the job being marked done with
		// nothing detected.
		return err
	}
	if err != nil {
		return err
	}

	faces := make([]StoredFace, 0, len(detections))
	for _, d := range detections {
		if !validBox(d.Box) || len(d.Vector) != h.detector.Dim() {
			// A malformed detection is dropped rather than failing the whole photo:
			// one bad box should not cost the other faces in the same image, and it
			// will not improve on a retry.
			h.log.Warn("dropping malformed detection", "node", *nodeID)
			continue
		}
		faces = append(faces, StoredFace{Box: d.Box, Vector: d.Vector})
	}

	// Stored even when empty, which is what records "this photo has been looked
	// at and has no faces" — otherwise every faceless photo is re-detected on
	// every reindex forever.
	if err := h.store.ReplaceFaces(ctx, ownerID, *nodeID,
		h.detector.Model(), h.detector.Dim(), faces); err != nil {
		return err
	}

	h.log.Info("detected faces", "node", *nodeID, "count", len(faces))
	return nil
}

// validBox rejects boxes outside the image or with no area.
func validBox(b [4]float64) bool {
	x, y, w, hh := b[0], b[1], b[2], b[3]
	if x < 0 || y < 0 || w <= 0 || hh <= 0 {
		return false
	}
	return x+w <= 1.0001 && y+hh <= 1.0001
}
