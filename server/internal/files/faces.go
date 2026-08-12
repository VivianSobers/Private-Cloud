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

	if _, err := tx.Exec(ctx,
		`DELETE FROM faces WHERE node_id = $1 AND model = $2`, nodeID, model); err != nil {
		return err
	}

	for i, f := range faces {
		if _, err := tx.Exec(ctx, `
			INSERT INTO faces (owner_id, node_id, box_x, box_y, box_w, box_h, model, dim, vector, seq)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			ownerID, nodeID, f.Box[0], f.Box[1], f.Box[2], f.Box[3],
			model, dim, embed.Pack(f.Vector), i); err != nil {
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

// ClusterUnassignedFaces assigns every unclustered face to a person.
func (s *Store) ClusterUnassignedFaces(ctx context.Context, ownerID uuid.UUID, model string) error {
	centroids, err := s.personCentroids(ctx, ownerID, model)
	if err != nil {
		return err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, vector FROM faces
		WHERE owner_id = $1 AND model = $2 AND person_id IS NULL
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
		for personID, centroid := range centroids {
			if score := embed.Cosine(p.vec, centroid); score > bestScore {
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
			centroids[bestID] = p.vec
			continue
		}

		if _, err := s.pool.Exec(ctx,
			`UPDATE faces SET person_id = $1 WHERE id = $2`, bestID, p.id); err != nil {
			return err
		}
		// The centroid moves toward the face just added, so a cluster tracks a
		// person across lighting and age rather than being pinned to whichever
		// photo happened to arrive first.
		centroids[bestID] = meanVector(centroids[bestID], p.vec)
	}
	return nil
}

// personCentroids computes the mean vector of each cluster.
func (s *Store) personCentroids(ctx context.Context, ownerID uuid.UUID, model string) (map[uuid.UUID][]float32, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT person_id, vector FROM faces
		WHERE owner_id = $1 AND model = $2 AND person_id IS NOT NULL
		LIMIT $3`, ownerID, model, MaxFaceScan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sums := map[uuid.UUID][]float32{}
	counts := map[uuid.UUID]int{}
	for rows.Next() {
		var (
			id  uuid.UUID
			raw []byte
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		v := embed.Unpack(raw)
		if cur, ok := sums[id]; ok {
			for i := range cur {
				if i < len(v) {
					cur[i] += v[i]
				}
			}
		} else {
			cp := make([]float32, len(v))
			copy(cp, v)
			sums[id] = cp
		}
		counts[id]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for id, sum := range sums {
		n := float32(counts[id])
		for i := range sum {
			sum[i] /= n
		}
	}
	return sums, nil
}

func meanVector(a, b []float32) []float32 {
	out := make([]float32, len(a))
	for i := range a {
		if i < len(b) {
			out[i] = (a[i] + b[i]) / 2
		} else {
			out[i] = a[i]
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
// detections in them — the faces simply become unassigned again.
func (s *Store) ForgetPerson(ctx context.Context, ownerID, personID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM people WHERE id = $1 AND owner_id = $2`, personID, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrPersonNotFound
	}
	return nil
}

// ReassignFace moves one face to a different cluster — the correction path for a
// single wrong detection, as opposed to merge's whole-cluster case.
//
// A nil personID detaches the face, which is how a user says "this is not a
// face" or "this is nobody I care about" without deleting the detection.
func (s *Store) ReassignFace(ctx context.Context, ownerID, faceID uuid.UUID, personID *uuid.UUID) error {
	if personID != nil {
		if _, err := s.GetPerson(ctx, ownerID, *personID); err != nil {
			return err
		}
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE faces SET person_id = $3 WHERE id = $1 AND owner_id = $2`,
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
