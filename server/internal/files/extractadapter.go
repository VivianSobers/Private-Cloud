package files

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
)

// NewExtractOpener adapts the file service to the extractor's Opener interface —
// the co-located path, where the worker runs on the same box and reads content
// through the blob store directly. A worker on a GPU box uses a different Opener
// that pulls the same bytes over the authenticated download API; the handler
// cannot tell them apart.
//
// files depends on extract, never the reverse: extract stays pure of the database
// so it can be tested on bytes alone, and files — which already knows how to open
// content — supplies the adapter.
func NewExtractOpener(svc *Service) extract.Opener {
	return extractOpener{svc: svc}
}

type extractOpener struct{ svc *Service }

func (o extractOpener) OpenForExtract(ctx context.Context, ownerID, nodeID uuid.UUID) (extract.FileContent, error) {
	node, rc, err := o.svc.Open(ctx, ownerID, nodeID)
	if errors.Is(err, ErrNotFound) {
		// Trashed or purged between enqueue and now: nothing to extract.
		return extract.FileContent{}, extract.ErrContentGone
	}
	if err != nil {
		return extract.FileContent{}, err
	}
	return extract.FileContent{
		MIME:        node.MIME,
		ContentHash: node.ContentHash,
		Reader:      rc,
	}, nil
}
