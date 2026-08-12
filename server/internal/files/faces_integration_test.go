package files_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Face clustering.
//
// The tests care about grouping behaviour and about the correction paths, not
// about detection quality — the vectors here are hand-made so "the same person"
// and "a different person" are unambiguous, which is the only way to assert what
// the clusterer did rather than what a model happened to produce.

// faceVec builds a unit-ish vector pointing mostly along one axis, with a small
// perturbation. Two vectors on the same axis are far above the match threshold;
// two on different axes are far below it.
func faceVec(axis int, jitter float32) []float32 {
	v := make([]float32, 16)
	v[axis%16] = 1
	v[(axis+1)%16] = jitter
	return v
}

func storeFaces(t *testing.T, f *fixture, nodeID uuid.UUID, vecs ...[]float32) {
	t.Helper()
	faces := make([]files.Face, 0, len(vecs))
	for _, v := range vecs {
		faces = append(faces, files.Face{Box: [4]float64{0.1, 0.1, 0.2, 0.2}, Vector: v})
	}
	if err := f.store.ReplaceFaces(context.Background(), f.user, nodeID, "test-face", 16, faces); err != nil {
		t.Fatalf("ReplaceFaces: %v", err)
	}
}

// TestCorrectionsSurviveTheNextUpload is the property the correction paths
// exist for and did not have.
//
// A dismissed face and a not-yet-clustered face were both person_id IS NULL, and
// ClusterUnassignedFaces claims exactly that, so every correction was reverted
// by the next photo the user uploaded. "Forget this person" rebuilt the cluster;
// "this is not a face" put the face back into one. A correction path that undoes
// itself on the user's next ordinary action is worse than none, because they
// stop believing the ones that do work.
func TestCorrectionsSurviveTheNextUpload(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a := f.upload(f.root, "forget-a.jpg", "a")
	b := f.upload(f.root, "forget-b.jpg", "b")
	storeFaces(t, f, a.ID, faceVec(3, 0.01))
	storeFaces(t, f, b.ID, faceVec(3, 0.02))

	people, err := f.store.ListPeople(ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 {
		t.Fatalf("setup produced %d clusters, want 1", len(people))
	}
	if err := f.store.ForgetPerson(ctx, f.user, people[0].ID); err != nil {
		t.Fatalf("ForgetPerson: %v", err)
	}

	// The ordinary next action: another photo arrives and is clustered.
	c := f.upload(f.root, "forget-c.jpg", "c")
	storeFaces(t, f, c.ID, faceVec(9, 0.01))

	people, err = f.store.ListPeople(ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range people {
		if p.FaceCount > 1 {
			t.Errorf("a forgotten cluster came back with %d faces", p.FaceCount)
		}
	}
	if len(people) != 1 {
		t.Errorf("got %d cluster(s) after forgetting one and adding one, want 1", len(people))
	}

	// The detections themselves are still there — forgetting a person must not
	// discard what the detector found, only the grouping.
	faces, err := f.store.FacesInNode(ctx, f.user, a.ID)
	if err != nil || len(faces) != 1 {
		t.Fatalf("detections lost by ForgetPerson: %d, err=%v", len(faces), err)
	}
	if faces[0].PersonID != nil {
		t.Error("a forgotten face was re-clustered")
	}
}

// Dismissing one face is the single-detection correction, and it has to hold for
// the same reason forgetting a person does.
func TestDismissedFaceIsNotReclustered(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a := f.upload(f.root, "dismiss-a.jpg", "a")
	storeFaces(t, f, a.ID, faceVec(11, 0.01))

	faces, err := f.store.FacesInNode(ctx, f.user, a.ID)
	if err != nil || len(faces) != 1 {
		t.Fatalf("setup: %d faces, err=%v", len(faces), err)
	}
	if err := f.store.ReassignFace(ctx, f.user, faces[0].ID, nil); err != nil {
		t.Fatalf("ReassignFace(nil): %v", err)
	}

	b := f.upload(f.root, "dismiss-b.jpg", "b")
	storeFaces(t, f, b.ID, faceVec(11, 0.02)) // would have matched the dismissed one

	faces, err = f.store.FacesInNode(ctx, f.user, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if faces[0].PersonID != nil {
		t.Error("a dismissed face was re-clustered by the next upload")
	}

	// Re-detection must not resurrect it either: seq identifies the same face
	// across runs, so an operator running a reindex does not undo the correction.
	storeFaces(t, f, a.ID, faceVec(11, 0.01))
	faces, err = f.store.FacesInNode(ctx, f.user, a.ID)
	if err != nil || len(faces) != 1 {
		t.Fatalf("after re-detection: %d faces, err=%v", len(faces), err)
	}
	if faces[0].PersonID != nil {
		t.Error("re-detection resurrected a dismissed face")
	}

	// Placing it on a person clears the dismissal — somebody who has just put a
	// face somewhere has plainly not dismissed it.
	people, err := f.store.ListPeople(ctx, f.user)
	if err != nil || len(people) == 0 {
		t.Fatalf("no cluster to reassign into: %d, err=%v", len(people), err)
	}
	if err := f.store.ReassignFace(ctx, f.user, faces[0].ID, &people[0].ID); err != nil {
		t.Fatalf("ReassignFace(person): %v", err)
	}
	faces, err = f.store.FacesInNode(ctx, f.user, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if faces[0].PersonID == nil {
		t.Error("reassigning a dismissed face to a person did not take")
	}
}

// TestCachedCentroidsStayHonest is the invariant a cache has to hold.
//
// The centroid moved into `people` so the clusterer stops averaging every face
// the owner has on every photo. The risk that buys is the usual one: a cached
// value that quietly stops describing what it claims to. Removing a face from a
// mean is not the inverse of adding one, so every operation that changes
// membership in a way the fold cannot express has to mark the cluster stale, and
// missing one of them is silent — the cluster simply starts matching the wrong
// people, weeks later.
//
// So the assertion is not about any one operation: it is that after EVERY
// mutation, a stored centroid either is absent or agrees with the faces that are
// actually there.
func TestCachedCentroidsStayHonest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Either centroid IS NULL — stale, and it will be rebuilt on next use — or
	// face_count is exactly the number of faces the cluster holds.
	checkCoherent := func(after string) {
		t.Helper()
		rows, err := f.store.Pool().Query(ctx, `
			SELECT p.id, p.centroid IS NULL, p.face_count,
			       (SELECT count(*) FROM faces WHERE person_id = p.id AND model = 'test-face')
			FROM people p WHERE p.owner_id = $1`, f.user)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id     uuid.UUID
				stale  bool
				cached int
				actual int
			)
			if err := rows.Scan(&id, &stale, &cached, &actual); err != nil {
				t.Fatal(err)
			}
			if !stale && cached != actual {
				t.Errorf("after %s: cluster %s caches %d faces but holds %d",
					after, id, cached, actual)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}

	a := f.upload(f.root, "coh-a.jpg", "a")
	b := f.upload(f.root, "coh-b.jpg", "b")
	c := f.upload(f.root, "coh-c.jpg", "c")
	storeFaces(t, f, a.ID, faceVec(4, 0.01), faceVec(4, 0.02))
	storeFaces(t, f, b.ID, faceVec(4, 0.03))
	storeFaces(t, f, c.ID, faceVec(12, 0.01))
	checkCoherent("clustering")

	people, err := f.store.ListPeople(ctx, f.user)
	if err != nil || len(people) != 2 {
		t.Fatalf("setup: %d cluster(s), err=%v", len(people), err)
	}

	if err := f.store.MergePeople(ctx, f.user, people[1].ID, people[0].ID); err != nil {
		t.Fatalf("MergePeople: %v", err)
	}
	checkCoherent("a merge")

	faces, err := f.store.FacesInNode(ctx, f.user, a.ID)
	if err != nil || len(faces) == 0 {
		t.Fatalf("FacesInNode: %d, err=%v", len(faces), err)
	}
	if err := f.store.ReassignFace(ctx, f.user, faces[0].ID, nil); err != nil {
		t.Fatalf("ReassignFace: %v", err)
	}
	checkCoherent("a dismissal")

	// Re-detection over a photo replaces its faces wholesale, so every cluster
	// that photo contributed to loses members.
	storeFaces(t, f, b.ID, faceVec(4, 0.04))
	checkCoherent("re-detection")

	// And a cluster rebuilt from stale still attracts the person it describes,
	// rather than having quietly become a different shape.
	d := f.upload(f.root, "coh-d.jpg", "d")
	storeFaces(t, f, d.ID, faceVec(4, 0.05))
	checkCoherent("clustering after invalidation")

	people, err = f.store.ListPeople(ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range people {
		if p.FaceCount == 1 && len(people) > 2 {
			t.Errorf("a rebuilt centroid stopped matching its own person: %d clusters", len(people))
			break
		}
	}
}

// A person's photos come back newest first, and each photo once however many
// faces of them it holds.
//
// The old query deduplicated with DISTINCT ON, which forces its expression to
// lead the ORDER BY — so the list came back in UUID order. Stable, and
// meaningless: paging through "photos of this person" put last week between 2019
// and 2004.
func TestPersonNodesAreNewestFirstAndDistinct(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var ids []uuid.UUID
	for _, name := range []string{"ord-1.jpg", "ord-2.jpg", "ord-3.jpg"} {
		n := f.upload(f.root, name, name)
		ids = append(ids, n.ID)
		// Two faces of the same person in each photo, which is what the removed
		// DISTINCT ON was there for.
		storeFaces(t, f, n.ID, faceVec(6, 0.01), faceVec(6, 0.02))
	}

	people, err := f.store.ListPeople(ctx, f.user)
	if err != nil || len(people) != 1 {
		t.Fatalf("setup: %d cluster(s), err=%v", len(people), err)
	}

	nodes, err := f.store.PersonNodes(ctx, f.user, people[0].ID, 50, 0)
	if err != nil {
		t.Fatalf("PersonNodes: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d photo(s), want 3 — each once", len(nodes))
	}
	for i := 1; i < len(nodes); i++ {
		if nodes[i].UpdatedAt.After(nodes[i-1].UpdatedAt) {
			t.Errorf("photo %d is newer than the one before it: not newest-first", i)
		}
	}
	// The most recently uploaded photo leads.
	if nodes[0].ID != ids[len(ids)-1] {
		t.Errorf("first result is %s, want the newest photo %s", nodes[0].Name, "ord-3.jpg")
	}
}

// The same person in two photos lands in one cluster; a different person starts
// their own.
func TestFacesClusterByPerson(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a := f.upload(f.root, "a.jpg", "a")
	b := f.upload(f.root, "b.jpg", "b")
	c := f.upload(f.root, "c.jpg", "c")

	storeFaces(t, f, a.ID, faceVec(0, 0.01))
	storeFaces(t, f, b.ID, faceVec(0, 0.02)) // same person
	storeFaces(t, f, c.ID, faceVec(5, 0.01)) // someone else

	people, err := f.store.ListPeople(ctx, f.user)
	if err != nil {
		t.Fatalf("ListPeople: %v", err)
	}
	if len(people) != 2 {
		t.Fatalf("got %d cluster(s), want 2", len(people))
	}
	// Largest first: the person in two photos.
	if people[0].FaceCount != 2 {
		t.Errorf("largest cluster has %d faces, want 2", people[0].FaceCount)
	}
	if people[0].Name != nil {
		t.Error("a new cluster is named — the system must never guess an identity")
	}
	if people[0].CoverNodeID == nil || people[0].CoverBox == nil {
		t.Error("a cluster has no cover face to crop a thumbnail from")
	}
}

// Re-running detection over a photo converges rather than accumulating.
func TestReplaceFacesDoesNotAccumulate(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "photo.jpg", "x")

	storeFaces(t, f, node.ID, faceVec(0, 0.01))
	storeFaces(t, f, node.ID, faceVec(0, 0.01))
	storeFaces(t, f, node.ID, faceVec(0, 0.01))

	faces, err := f.store.FacesInNode(context.Background(), f.user, node.ID)
	if err != nil {
		t.Fatalf("FacesInNode: %v", err)
	}
	if len(faces) != 1 {
		t.Fatalf("got %d faces after three detections of one face, want 1", len(faces))
	}
}

func TestNamingAndClearingAPerson(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	node := f.upload(f.root, "a.jpg", "a")
	storeFaces(t, f, node.ID, faceVec(0, 0.01))

	people, _ := f.store.ListPeople(ctx, f.user)
	id := people[0].ID

	name := "Ada"
	p, err := f.store.NamePerson(ctx, f.user, id, &name)
	if err != nil {
		t.Fatalf("NamePerson: %v", err)
	}
	if p.Name == nil || *p.Name != "Ada" {
		t.Fatalf("name = %v, want Ada", p.Name)
	}

	// Clearing it returns the cluster to unnamed rather than to an empty name.
	p, err = f.store.NamePerson(ctx, f.user, id, nil)
	if err != nil {
		t.Fatalf("clear name: %v", err)
	}
	if p.Name != nil {
		t.Errorf("name = %v after clearing, want nil", p.Name)
	}
}

// Merge is the whole-cluster correction path, and it must not lose faces.
func TestMergingTwoClustersKeepsEveryFace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a := f.upload(f.root, "a.jpg", "a")
	b := f.upload(f.root, "b.jpg", "b")
	storeFaces(t, f, a.ID, faceVec(0, 0.01))
	storeFaces(t, f, b.ID, faceVec(8, 0.01))

	people, _ := f.store.ListPeople(ctx, f.user)
	if len(people) != 2 {
		t.Fatalf("expected two clusters to merge, got %d", len(people))
	}
	from, into := people[0].ID, people[1].ID

	if err := f.store.MergePeople(ctx, f.user, from, into); err != nil {
		t.Fatalf("MergePeople: %v", err)
	}

	people, _ = f.store.ListPeople(ctx, f.user)
	if len(people) != 1 {
		t.Fatalf("got %d cluster(s) after merge, want 1", len(people))
	}
	if people[0].FaceCount != 2 {
		t.Errorf("merged cluster has %d faces, want 2 — merging lost a detection", people[0].FaceCount)
	}
	if _, err := f.store.GetPerson(ctx, f.user, from); !errors.Is(err, files.ErrPersonNotFound) {
		t.Error("the emptied cluster survived the merge")
	}
}

// Forgetting a cluster keeps the photographs AND the detections; the faces just
// become unassigned. Re-running detection to recover them would be pure waste.
func TestForgettingAPersonKeepsTheFaces(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	node := f.upload(f.root, "a.jpg", "a")
	storeFaces(t, f, node.ID, faceVec(0, 0.01))

	people, _ := f.store.ListPeople(ctx, f.user)
	if err := f.store.ForgetPerson(ctx, f.user, people[0].ID); err != nil {
		t.Fatalf("ForgetPerson: %v", err)
	}

	faces, err := f.store.FacesInNode(ctx, f.user, node.ID)
	if err != nil {
		t.Fatalf("FacesInNode: %v", err)
	}
	if len(faces) != 1 {
		t.Fatalf("forgetting a cluster deleted its detections: %d left", len(faces))
	}
	if faces[0].PersonID != nil {
		t.Error("the face still points at a deleted cluster")
	}
	// And the photo is untouched.
	if _, err := f.store.Get(ctx, f.user, node.ID); err != nil {
		t.Errorf("the photo was deleted with the cluster: %v", err)
	}
}

// Reassign is the single-face correction path, including detaching a
// misdetection without deleting it.
func TestReassigningAFace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a := f.upload(f.root, "a.jpg", "a")
	b := f.upload(f.root, "b.jpg", "b")
	storeFaces(t, f, a.ID, faceVec(0, 0.01))
	storeFaces(t, f, b.ID, faceVec(8, 0.01))

	people, _ := f.store.ListPeople(ctx, f.user)
	target := people[1].ID

	faces, _ := f.store.FacesInNode(ctx, f.user, a.ID)
	if err := f.store.ReassignFace(ctx, f.user, faces[0].ID, &target); err != nil {
		t.Fatalf("ReassignFace: %v", err)
	}
	faces, _ = f.store.FacesInNode(ctx, f.user, a.ID)
	if faces[0].PersonID == nil || *faces[0].PersonID != target {
		t.Fatalf("face was not reassigned: %v", faces[0].PersonID)
	}

	// Detaching leaves the detection in place.
	if err := f.store.ReassignFace(ctx, f.user, faces[0].ID, nil); err != nil {
		t.Fatalf("detach: %v", err)
	}
	faces, _ = f.store.FacesInNode(ctx, f.user, a.ID)
	if len(faces) != 1 || faces[0].PersonID != nil {
		t.Error("detaching a face deleted it or left it assigned")
	}
}

// The people graph is per owner. Two users owning the same photograph must not
// share clusters, or naming a face in your library would name it in a
// stranger's.
func TestPeopleAreNotSharedBetweenUsers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	node := f.upload(f.root, "a.jpg", "a")
	storeFaces(t, f, node.ID, faceVec(0, 0.01))

	people, err := f.store.ListPeople(ctx, f.other(t))
	if err != nil {
		t.Fatalf("ListPeople: %v", err)
	}
	if len(people) != 0 {
		t.Fatalf("another user sees %d of this owner's clusters", len(people))
	}

	mine, _ := f.store.ListPeople(ctx, f.user)
	if _, err := f.store.GetPerson(ctx, f.other(t), mine[0].ID); !errors.Is(err, files.ErrPersonNotFound) {
		t.Error("another user read this owner's cluster by id")
	}
	if err := f.store.ForgetPerson(ctx, f.other(t), mine[0].ID); !errors.Is(err, files.ErrPersonNotFound) {
		t.Error("another user deleted this owner's cluster")
	}
}

// A photo with no faces is still recorded as looked-at: an empty detection set
// must be storable, or every faceless photo is re-detected on every reindex.
func TestAPhotoWithNoFacesIsRecorded(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "landscape.jpg", "x")

	if err := f.store.ReplaceFaces(context.Background(), f.user, node.ID, "test-face", 16, nil); err != nil {
		t.Fatalf("ReplaceFaces with no faces: %v", err)
	}
	faces, err := f.store.FacesInNode(context.Background(), f.user, node.ID)
	if err != nil {
		t.Fatalf("FacesInNode: %v", err)
	}
	if len(faces) != 0 {
		t.Errorf("got %d faces for a photo with none", len(faces))
	}
}
