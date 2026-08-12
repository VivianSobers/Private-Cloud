package files

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/media"
)

// Adapters joining the media package to this one.
//
// files depends on media, never the reverse — the same rule extractadapter.go
// follows. media stays pure of the database so it can be tested on bytes alone,
// and files, which already knows how to open content and write blobs, supplies
// the glue.

// NewMediaOpener adapts the file service to the media handler's Opener.
func NewMediaOpener(svc *Service) media.Opener { return mediaOpener{svc: svc} }

type mediaOpener struct{ svc *Service }

func (o mediaOpener) OpenForMedia(ctx context.Context, ownerID, nodeID uuid.UUID) (media.FileContent, error) {
	node, rc, err := o.svc.Open(ctx, ownerID, nodeID)
	if errors.Is(err, ErrNotFound) {
		// Trashed or purged between enqueue and now.
		return media.FileContent{}, media.ErrContentGone
	}
	if err != nil {
		return media.FileContent{}, err
	}
	return media.FileContent{
		MIME:        node.MIME,
		ContentHash: node.ContentHash,
		Reader:      rc,
	}, nil
}

// NewMediaBlobWriter adapts the blob store to the handler's BlobWriter.
//
// Variant bytes go through the ordinary Put, so they are content-addressed and
// two identical thumbnails collapse to one file on disk without anything here
// having to notice.
func NewMediaBlobWriter(svc *Service) media.BlobWriter { return mediaBlobs{svc: svc} }

type mediaBlobs struct{ svc *Service }

func (b mediaBlobs) PutBytes(ctx context.Context, data []byte) (string, error) {
	res, err := b.svc.blobs.Put(ctx, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	return res.Key, nil
}

func (b mediaBlobs) Remove(ctx context.Context, key string) error {
	return b.svc.blobs.Delete(ctx, key)
}

// NewMediaStore adapts this package's store to the handler's VariantStore.
//
// The conversion between media.Metadata and MediaMeta is the price of keeping
// the dependency one-directional. It is a few lines in one place, which is
// cheaper than the import cycle the alternative would create.
func NewMediaStore(s *Store) media.VariantStore { return mediaStore{s: s} }

type mediaStore struct{ s *Store }

func (m mediaStore) HasMediaMeta(ctx context.Context, contentHash []byte) (bool, error) {
	return m.s.HasMediaMeta(ctx, contentHash)
}

func (m mediaStore) PutMediaMeta(ctx context.Context, contentHash []byte, meta media.Meta) error {
	return m.s.PutMediaMeta(ctx, contentHash, MediaMeta{
		Width:       meta.Width,
		Height:      meta.Height,
		Orientation: meta.Orientation,
		TakenAt:     meta.TakenAt,
		Camera:      meta.Camera,
		GPSLat:      meta.GPSLat,
		GPSLon:      meta.GPSLon,
		DurationMS:  meta.DurationMS,
		Source:      meta.Source,
	})
}

func (m mediaStore) PutMediaVariant(ctx context.Context, contentHash []byte, v media.StoredVariant) (string, error) {
	return m.s.PutMediaVariant(ctx, contentHash, MediaVariant{
		Variant:    v.Variant,
		StorageKey: v.StorageKey,
		MIME:       v.MIME,
		Size:       v.Size,
		Width:      v.Width,
		Height:     v.Height,
	})
}

// OpenMediaVariant returns a rendition's bytes for one of the owner's files,
// alongside the row describing it. ErrNotFound when the variant has not been
// rendered — the handler turns that into a 404 the client can retry, rather
// than silently serving a full-size original in its place.
func (s *Service) OpenMediaVariant(ctx context.Context, ownerID, nodeID uuid.UUID, variant string) (*MediaVariant, io.ReadSeekCloser, error) {
	v, err := s.store.MediaVariantFor(ctx, ownerID, nodeID, variant)
	if err != nil {
		return nil, nil, err
	}
	rc, err := s.blobs.Open(ctx, v.StorageKey)
	if err != nil {
		// The row survived its bytes. That is the direction GC never produces,
		// so it means something outside GC removed the file.
		return nil, nil, err
	}
	return v, rc, nil
}
