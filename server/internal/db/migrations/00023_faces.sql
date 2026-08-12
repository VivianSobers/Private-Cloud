-- +goose Up
-- Phase 8: face detection and clustering ("people").
--
-- UNLIKE doc_embedding, this is NOT content-addressed, and the difference is
-- deliberate. A document's vector describes its bytes and two users owning the
-- same file may safely share one row. A "people" graph describes who someone
-- knows: two users owning the same photograph must not share clusters, or naming
-- a face in your library would name it in a stranger's. Every row here is
-- per-owner.

CREATE TABLE people (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- NULL until a person names the cluster. The system never guesses an
    -- identity: an unnamed cluster is an honest "here are faces that look alike",
    -- while a guessed name is a claim about a real person that nobody made.
    name text,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX people_owner ON people (owner_id, updated_at DESC);

CREATE TABLE faces (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    node_id  uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,

    -- SET NULL, not CASCADE: deleting a cluster must forget the grouping, never
    -- the detections. The faces are still in the photographs, and re-running
    -- detection to recover them would be pure waste.
    person_id uuid REFERENCES people (id) ON DELETE SET NULL,

    -- Bounding box as FRACTIONS of the image, not pixels, so a client can crop
    -- from whichever variant it already has — a thumbnail, a preview or the
    -- original — without knowing which one the detector saw.
    box_x double precision NOT NULL CHECK (box_x >= 0 AND box_x <= 1),
    box_y double precision NOT NULL CHECK (box_y >= 0 AND box_y <= 1),
    box_w double precision NOT NULL CHECK (box_w > 0 AND box_w <= 1),
    box_h double precision NOT NULL CHECK (box_h > 0 AND box_h <= 1),

    -- The face embedding, packed little-endian float32 exactly as doc_embedding
    -- stores document vectors. Same reasoning: no pgvector on the stock image,
    -- and at personal scale an exact scan is fast and always correct.
    model  text  NOT NULL,
    dim    int   NOT NULL CHECK (dim > 0),
    vector bytea NOT NULL CHECK (octet_length(vector) = dim * 4),

    -- Detection order within one photo, so re-running detection can replace a
    -- node's faces deterministically rather than accumulating duplicates.
    seq int NOT NULL,

    detected_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (node_id, model, seq)
);

-- "Which faces belong to this person?" — the /people/{id} read.
CREATE INDEX faces_person ON faces (person_id) WHERE person_id IS NOT NULL;
-- "Which faces are in this photo?" — the reassign path and re-detection.
CREATE INDEX faces_node ON faces (node_id);
-- The clustering scan: one owner's faces in one model's space.
CREATE INDEX faces_owner_model ON faces (owner_id, model);

-- +goose Down
DROP TABLE IF EXISTS faces;
DROP TABLE IF EXISTS people;
