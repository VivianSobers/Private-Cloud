-- +goose Up
-- A cluster's centroid, kept rather than recomputed.
--
-- ClusterUnassignedFaces runs once per analysed photo and began by averaging
-- every clustered face vector the owner has, up to MaxFaceScan of them. That is
-- O(N) work per photo and O(N^2) to import a library — for an answer that
-- changes by one face each time.
--
-- centroid is the packed float32 mean, in the same little-endian layout
-- faces.vector and doc_embedding.vector use. face_count is how many faces it
-- averages, which is what lets the mean be folded forward by 1/(n+1) instead of
-- recomputed.
--
-- NULL centroid means STALE, not empty, and is the only invalidation mechanism:
-- anything that changes a cluster's membership in a way that cannot be folded
-- forward — a merge, a reassignment, a dismissal, re-detection over a photo —
-- sets it back to NULL and the next pass recomputes that ONE cluster from its
-- faces. Existing rows start NULL, so no backfill is needed.
--
-- centroid_model records which embedding space it was computed in. A cluster is
-- not per-model but a vector is, so a deployment that changes face models must
-- not compare new vectors against a mean built from the old one's.
ALTER TABLE people
    ADD COLUMN centroid       bytea,
    ADD COLUMN centroid_model text,
    ADD COLUMN face_count     int NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE people
    DROP COLUMN IF EXISTS face_count,
    DROP COLUMN IF EXISTS centroid_model,
    DROP COLUMN IF EXISTS centroid;
