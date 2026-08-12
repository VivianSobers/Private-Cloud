-- +goose Up
-- A face a person has DISMISSED, as opposed to one not yet clustered.
--
-- Both were person_id IS NULL, and ClusterUnassignedFaces selects exactly that,
-- so every user correction was reverted by the next photo they uploaded.
-- "Forget this person" deleted the cluster, ON DELETE SET NULL detached its
-- faces, and the next detection job put them straight back into a new cluster.
-- Detaching a single face - documented as how somebody says "this is not a face"
-- or "this is nobody I care about" - lasted exactly as long.
--
-- A tombstone rather than deleting the row, for the same reason ON DELETE SET
-- NULL exists on person_id: the face is still in the photograph, and re-running
-- detection to rediscover it would be pure waste. It also means a dismissal
-- survives re-detection under a new model, which is when a user is least
-- inclined to redo work they have already done once.
--
-- NULL means "not dismissed", which is the overwhelmingly common case and costs
-- nothing to store.
ALTER TABLE faces ADD COLUMN dismissed_at timestamptz;

-- The clustering scan reads unassigned, undismissed faces for one owner and
-- model. Partial, because the rows it must find are the minority and the index
-- should not carry the rest.
CREATE INDEX faces_unclustered ON faces (owner_id, model, detected_at, seq)
    WHERE person_id IS NULL AND dismissed_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS faces_unclustered;
ALTER TABLE faces DROP COLUMN IF EXISTS dismissed_at;
