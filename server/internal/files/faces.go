package files

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
)

// Faces and people (Phase 8).
//
// Clustering is INCREMENTAL and greedy: each new face joins the nearest existing
// cluster whose centroid is close enough, or starts one of its own. Not a global
// k-means or a full agglomerative pass, and the reason is operational rather
// than mathematical — a photo library grows one upload at a time, and a scheme
// that re-partitions everything on each arrival would either run constantly or
// keep renaming the clusters a person has already named. A name is a promise;
// re-clustering must not silently break it.
//
// The consequence is that clustering WILL be wrong sometimes: greedy assignment
// depends on arrival order. That is why merge and reassign exist, and why they
// are part of the design rather than an afterthought — a faces feature with no
// correction path is one people stop trusting after the first mistake.

// FaceMatchThreshold is the cosine similarity above which a face is considered
// the same person as a cluster.
//
// Deliberately conservative. Too low merges two people into one cluster, which a
// user experiences as the feature being wrong about who someone is; too high
// scatters one person across several clusters, which they experience as the
// feature being incomplete. The second is much easier to fix — merge is one
// click — so the threshold errs toward splitting.
const FaceMatchThreshold = 0.72

// MaxFaceScan bounds the clustering comparison, like maxSemanticScan.
const MaxFaceScan = 50000

var (
	// ErrPersonNotFound means no such cluster, or it is not the caller's.
	ErrPersonNotFound = errors.New("person not found")
	// ErrFaceNotFound means no such face for this caller.
	ErrFaceNotFound = errors.New("face not found")
)

// Face is one detected face.
type Face struct {
	ID       uuid.UUID
	NodeID   uuid.UUID
	PersonID *uuid.UUID
	Box      [4]float64 // x, y, w, h as fractions
	Seq      int
	Vector   []float32
}

// Person is a cluster of faces.
type Person struct {
	ID        uuid.UUID
	Name      *string
	FaceCount int
	// Cover names a face to crop a thumbnail from, so a UI can show who a cluster
	// is without loading every photo in it.
	CoverNodeID *uuid.UUID
	CoverBox    *[4]float64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ReplaceFaces stores the faces detected in one node, replacing any previous
// detection for the same model, and assigns each to a cluster.
//
// Replace rather than append: re-running detection over a photo must converge on
// the same set rather than accumulating a duplicate face per run.
func (s *Store) ReplaceFaces(ctx context.Context, ownerID, nodeID uuid.UUID, model string, dim int, faces []Face) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Dismissals survive re-detection. Detection is deterministic for a given
	// model, so seq identifies the same face across runs, and somebody who has
	// already said "this is not a face" should not have to say it again because
	// an operator ran a reindex. Read before the delete, which is what would
	// otherwise lose them.
	dismissed := map[int]bool{}
	rows, err := tx.Query(ctx,
		`SELECT seq FROM faces WHERE node_id = $1 AND model = $2 AND dismissed_at IS NOT NULL`,
		nodeID, model)
	if err != nil {
		return err
	}
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return err
		}
		dismissed[seq] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM faces WHERE node_id = $1 AND model = $2`, nodeID, model); err != nil {
		return err
	}

	for i, f := range faces {
		var dismissedAt *time.Time
		if dismissed[i] {
			now := time.Now()
			dismissedAt = &now
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO faces (owner_id, node_id, box_x, box_y, box_w, box_h, model, dim, vector, seq, dismissed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			ownerID, nodeID, f.Box[0], f.Box[1], f.Box[2], f.Box[3],
			model, dim, embed.Pack(f.Vector), i, dismissedAt); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// Clustering runs after the commit, on its own, so a slow assignment pass
	// never holds the write transaction open — and so a failure to cluster leaves
	// the detections stored rather than losing them. An unclustered face is
	// recoverable by re-running; a lost detection is not.
	return s.ClusterUnassignedFaces(ctx, ownerID, model)
}

// cluster is a person's running centroid and how many faces it averages.
//
// The count is what makes the centroid update honest — see the note on the
// assignment loop below.
type cluster struct {
	centroid []float32
	count    int
}

// ClusterUnassignedFaces assigns every unclustered face to a person.
func (s *Store) ClusterUnassignedFaces(ctx context.Context, ownerID uuid.UUID, model string) error {
	clusters, err := s.personCentroids(ctx, ownerID, model)
	if err != nil {
		return err
	}

	// dismissed_at IS NULL is what makes a correction stick. A dismissed face and
	// a not-yet-clustered face were both person_id IS NULL, so every "forget this
	// person" and every "this is not a face" was undone by the next photo the
	// user uploaded — the clustering pass simply found them again.
	rows, err := s.pool.Query(ctx, `
		SELECT id, vector FROM faces
		WHERE owner_id = $1 AND model = $2
		  AND person_id IS NULL AND dismissed_at IS NULL
		ORDER BY detected_at, seq
		LIMIT $3`, ownerID, model, MaxFaceScan)
	if err != nil {
		return err
	}
	type pending struct {
		id  uuid.UUID
		vec []float32
	}
	var todo []pending
	for rows.Next() {
		var (
			id  uuid.UUID
			raw []byte
		)
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, pending{id: id, vec: embed.Unpack(raw)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, p := range todo {
		bestID, bestScore := uuid.Nil, FaceMatchThreshold
		for personID, c := range clusters {
			if score := embed.Cosine(p.vec, c.centroid); score > bestScore {
				bestID, bestScore = personID, score
			}
		}

		if bestID == uuid.Nil {
			// Nothing close enough: a new, unnamed cluster. Unnamed because the
			// system never guesses an identity.
			if err := s.pool.QueryRow(ctx, `
				WITH p AS (
					INSERT INTO people (owner_id) VALUES ($1) RETURNING id
				)
				UPDATE faces SET person_id = (SELECT id FROM p)
				WHERE id = $2
				RETURNING person_id`, ownerID, p.id).Scan(&bestID); err != nil {
				return err
			}
			clusters[bestID] = &cluster{centroid: p.vec, count: 1}
			continue
		}

		if _, err := s.pool.Exec(ctx,
			`UPDATE faces SET person_id = $1 WHERE id = $2`, bestID, p.id); err != nil {
			return err
		}
		// The centroid moves toward the face just added, so a cluster tracks a
		// person across lighting and age rather than being pinned to whichever
		// photo happened to arrive first — but it moves by 1/(n+1), not halfway.
		//
		// An unweighted midpoint made every new face worth as much as the entire
		// cluster it joined, so one borderline match dragged a hundred-photo
		// centroid halfway to itself. That is how a cluster drifts off the person
		// it is supposed to be: the next face matches the drifted centroid rather
		// than anyone in particular, and the error compounds one photo at a time
		// until a named cluster is full of strangers. Naming is a promise, and
		// this is the arithmetic that has to keep it.
		c := clusters[bestID]
		c.centroid = weightedMean(c.centroid, c.count, p.vec)
		c.count++
	}
	return nil
}

// personCentroids computes each cluster's mean vector and its size.
//
// The size is returned, not discarded: it is what lets the assignment loop move
// a centroid by 1/(n+1) rather than halfway. An ORDER BY on the scan so that a
// library past MaxFaceScan truncates deterministically — an arbitrary subset
// would make clustering depend on Postgres's row order, which is to say on
// nothing.
func (s *Store) personCentroids(ctx context.Context, ownerID uuid.UUID, model string) (map[uuid.UUID]*cluster, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT person_id, vector FROM faces
		WHERE owner_id = $1 AND model = $2 AND person_id IS NOT NULL
		ORDER BY person_id, detected_at, seq
		LIMIT $3`, ownerID, model, MaxFaceScan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uuid.UUID]*cluster{}
	for rows.Next() {
		var (
			id  uuid.UUID
			raw []byte
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		v := embed.Unpack(raw)
		c, ok := out[id]
		if !ok {
			cp := make([]float32, len(v))
			copy(cp, v)
			out[id] = &cluster{centroid: cp, count: 1}
			continue
		}
		// A running mean rather than sum-then-divide: the vectors are float32 and
		// a cluster can hold thousands, so a sum is the one place this loses
		// precision for no reason.
		c.centroid = weightedMean(c.centroid, c.count, v)
		c.count++
	}
	return out, rows.Err()
}

// weightedMean folds one more vector into a mean of n, moving it by 1/(n+1).
//
// Deliberately not a midpoint. Averaging a cluster's centroid with a single new
// face makes that face worth as much as everyone already in the cluster, which
// is only correct when the cluster has exactly one member.
func weightedMean(mean []float32, n int, v []float32) []float32 {
	out := make([]float32, len(mean))
	w := float32(n)
	for i := range mean {
		if i < len(v) {
			out[i] = (mean[i]*w + v[i]) / (w + 1)
		} else {
			out[i] = mean[i]
		}
	}
	return out
}

// ListPeople returns the owner's clusters, largest first.
//
// Largest first because a person photographed a hundred times is who someone is
// looking for; a one-off cluster from a stranger in the background is noise.
func (s *Store) ListPeople(ctx context.Context, ownerID uuid.UUID) ([]*Person, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.name, p.created_at, p.updated_at,
		       count(f.id),
		       (array_agg(f.node_id ORDER BY f.detected_at))[1],
		       -- Four scalar aggregates rather than array_agg over an ARRAY[...]:
		       -- that would build a 2-D array, and subscripting one with a single
		       -- index yields NULL in Postgres rather than the first row.
		       (array_agg(f.box_x ORDER BY f.detected_at))[1],
		       (array_agg(f.box_y ORDER BY f.detected_at))[1],
		       (array_agg(f.box_w ORDER BY f.detected_at))[1],
		       (array_agg(f.box_h ORDER BY f.detected_at))[1]
		FROM people p
		LEFT JOIN faces f ON f.person_id = p.id
		WHERE p.owner_id = $1
		GROUP BY p.id
		HAVING count(f.id) > 0
		ORDER BY count(f.id) DESC, p.created_at`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Person, 0, 16)
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPerson returns one cluster, verifying ownership.
func (s *Store) GetPerson(ctx context.Context, ownerID, personID uuid.UUID) (*Person, error) {
	p, err := scanPerson(s.pool.QueryRow(ctx, `
		SELECT p.id, p.name, p.created_at, p.updated_at,
		       count(f.id),
		       (array_agg(f.node_id ORDER BY f.detected_at))[1],
		       -- Four scalar aggregates rather than array_agg over an ARRAY[...]:
		       -- that would build a 2-D array, and subscripting one with a single
		       -- index yields NULL in Postgres rather than the first row.
		       (array_agg(f.box_x ORDER BY f.detected_at))[1],
		       (array_agg(f.box_y ORDER BY f.detected_at))[1],
		       (array_agg(f.box_w ORDER BY f.detected_at))[1],
		       (array_agg(f.box_h ORDER BY f.detected_at))[1]
		FROM people p
		LEFT JOIN faces f ON f.person_id = p.id
		WHERE p.id = $1 AND p.owner_id = $2
		GROUP BY p.id`, personID, ownerID))
	if err != nil {
		return nil, err
	}
	return p, nil
}

func scanPerson(row pgx.Row) (*Person, error) {
	var (
		p          Person
		nodeID     *uuid.UUID
		x, y, w, h *float64
	)
	err := row.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt, &p.FaceCount,
		&nodeID, &x, &y, &w, &h)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPersonNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CoverNodeID = nodeID
	if x != nil && y != nil && w != nil && h != nil {
		p.CoverBox = &[4]float64{*x, *y, *w, *h}
	}
	return &p, nil
}

// PersonNodes returns the photos a cluster appears in.
func (s *Store) PersonNodes(ctx context.Context, ownerID, personID uuid.UUID, limit, offset int) ([]*Node, error) {
	if _, err := s.GetPerson(ctx, ownerID, personID); err != nil {
		return nil, err
	}
	limit = ClampSearchLimit(limit)
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (n.id) `+nodeCols+`
		`+nodeFrom+`
		JOIN faces f ON f.node_id = n.id
		WHERE f.person_id = $1 AND n.owner_id = $2 AND n.trashed_at IS NULL
		ORDER BY n.id, n.updated_at DESC
		LIMIT $3 OFFSET $4`, personID, ownerID, limit, clampOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// NamePerson gives a cluster a name. An empty name clears it back to unnamed.
func (s *Store) NamePerson(ctx context.Context, ownerID, personID uuid.UUID, name *string) (*Person, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE people SET name = $3, updated_at = now()
		WHERE id = $1 AND owner_id = $2`, personID, ownerID, name)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrPersonNotFound
	}
	return s.GetPerson(ctx, ownerID, personID)
}

// MergePeople folds one cluster into another.
//
// Exists because clustering IS going to be wrong: greedy incremental assignment
// scatters one person across several clusters whenever lighting or age changes
// enough. Without a correction path people stop trusting the feature after the
// first mistake.
func (s *Store) MergePeople(ctx context.Context, ownerID, from, into uuid.UUID) error {
	if from == into {
		return nil
	}
	// Both must be the caller's, checked before anything moves.
	if _, err := s.GetPerson(ctx, ownerID, from); err != nil {
		return err
	}
	if _, err := s.GetPerson(ctx, ownerID, into); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE faces SET person_id = $2 WHERE person_id = $1 AND owner_id = $3`,
		from, into, ownerID); err != nil {
		return err
	}
	// The emptied cluster goes; its faces are all accounted for in the target.
	if _, err := tx.Exec(ctx,
		`DELETE FROM people WHERE id = $1 AND owner_id = $2`, from, ownerID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE people SET updated_at = now() WHERE id = $1`, into); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ForgetPerson deletes a cluster without touching the photographs or the
// detections in them.
//
// Its faces are DISMISSED, not merely unassigned. Unassigned is the state a
// brand-new detection is in, so leaving them there meant the next clustering
// pass rebuilt the cluster the user had just asked to be rid of — the request
// undone by the next photo they uploaded. The detections themselves survive,
// because the faces are still in the photographs and re-running detection to
// rediscover them would be pure waste.
//
// A transaction rather than one CTE statement: the delete's ON DELETE SET NULL
// writes the same rows the dismissal does, and Postgres refuses two updates to a
// row from one command. Two statements in order, and the dismissal must be
// inside the transaction — a crash between them would leave faces the next pass
// re-clusters, which is the bug this is fixing.
func (s *Store) ForgetPerson(ctx context.Context, ownerID, personID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx,
		`UPDATE faces SET dismissed_at = now() WHERE person_id = $1 AND owner_id = $2`,
		personID, ownerID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM people WHERE id = $1 AND owner_id = $2`, personID, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrPersonNotFound
	}
	return tx.Commit(ctx)
}

// ReassignFace moves one face to a different cluster — the correction path for a
// single wrong detection, as opposed to merge's whole-cluster case.
//
// A nil personID DISMISSES the face, which is how a user says "this is not a
// face" or "this is nobody I care about" without deleting the detection.
//
// Dismissing has to be distinguishable from never-clustered, because the
// clustering pass claims everything unassigned. Setting person_id to NULL and
// stopping there meant the next photo uploaded put the face straight back into a
// cluster, so the correction lasted until the user's next upload.
//
// Assigning to a person clears the tombstone in the same statement: a face
// someone has just placed is, by definition, not one they have dismissed.
func (s *Store) ReassignFace(ctx context.Context, ownerID, faceID uuid.UUID, personID *uuid.UUID) error {
	if personID != nil {
		if _, err := s.GetPerson(ctx, ownerID, *personID); err != nil {
			return err
		}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE faces
		SET person_id = $3,
		    dismissed_at = CASE WHEN $3::uuid IS NULL THEN now() ELSE NULL END
		WHERE id = $1 AND owner_id = $2`,
		faceID, ownerID, personID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrFaceNotFound
	}
	return nil
}

// FacesInNode lists the faces detected in one photo, for a "who is in this
// picture" overlay and for the reassign path.
func (s *Store) FacesInNode(ctx context.Context, ownerID, nodeID uuid.UUID) ([]*Face, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, node_id, person_id, box_x, box_y, box_w, box_h, seq
		FROM faces
		WHERE node_id = $1 AND owner_id = $2
		ORDER BY seq`, nodeID, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Face, 0, 4)
	for rows.Next() {
		var f Face
		if err := rows.Scan(&f.ID, &f.NodeID, &f.PersonID,
			&f.Box[0], &f.Box[1], &f.Box[2], &f.Box[3], &f.Seq); err != nil {
			return nil, err
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}
