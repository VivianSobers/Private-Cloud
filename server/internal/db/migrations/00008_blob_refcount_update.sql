-- +goose Up
-- Phase 2, slice 2: let the blob refcount trigger survive an in-place format
-- switch.
--
-- Background migration rewrites a whole-file version as content-addressed
-- chunks by UPDATE-ing file_versions in place — blob_id -> NULL, manifest_id
-- set — because "only what it points at changes" (see 00003). The refcount
-- trigger from 00003 fires on INSERT and DELETE only, so that UPDATE would move
-- the reference off the blob WITHOUT decrementing its count. The blob would then
-- sit at refcount 1 forever, and GC — which only reclaims blobs at zero — would
-- never free the bytes the migration just made redundant. A permanent leak, in
-- exactly the phase whose point is to store LESS.
--
-- The fix is to teach the trigger about UPDATE. It stays behaviour-identical for
-- the INSERT/DELETE paths: the manifest path inserts rows with blob_id NULL, and
-- `WHERE id = NULL` already matched nothing, so the added guards only make that
-- explicit. Migration is the first thing in the system that ever mutates a
-- version's storage pointer, which is why this could wait until now.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION blob_refcount_bump() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.blob_id IS NOT NULL THEN
            UPDATE blobs SET refcount = refcount + 1 WHERE id = NEW.blob_id;
        END IF;
    ELSIF TG_OP = 'DELETE' THEN
        IF OLD.blob_id IS NOT NULL THEN
            UPDATE blobs SET refcount = refcount - 1 WHERE id = OLD.blob_id;
        END IF;
    ELSIF TG_OP = 'UPDATE' THEN
        -- Only when the reference actually moves. IS DISTINCT FROM is NULL-safe,
        -- so a version going blob -> manifest (NULL) decrements the old blob and
        -- credits nothing, which is precisely the migration's intent.
        IF OLD.blob_id IS DISTINCT FROM NEW.blob_id THEN
            IF OLD.blob_id IS NOT NULL THEN
                UPDATE blobs SET refcount = refcount - 1 WHERE id = OLD.blob_id;
            END IF;
            IF NEW.blob_id IS NOT NULL THEN
                UPDATE blobs SET refcount = refcount + 1 WHERE id = NEW.blob_id;
            END IF;
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS file_versions_refcount ON file_versions;
CREATE TRIGGER file_versions_refcount
    AFTER INSERT OR UPDATE OR DELETE ON file_versions
    FOR EACH ROW EXECUTE FUNCTION blob_refcount_bump();

-- +goose Down
-- Restore the INSERT/DELETE-only trigger. Safe only because nothing but the
-- migration UPDATEs a version's storage pointer, and rolling back the trigger
-- does not touch data — it just stops future in-place switches from being
-- counted, which is the pre-migration behaviour.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION blob_refcount_bump() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE blobs SET refcount = refcount + 1 WHERE id = NEW.blob_id;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE blobs SET refcount = refcount - 1 WHERE id = OLD.blob_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS file_versions_refcount ON file_versions;
CREATE TRIGGER file_versions_refcount
    AFTER INSERT OR DELETE ON file_versions
    FOR EACH ROW EXECUTE FUNCTION blob_refcount_bump();
