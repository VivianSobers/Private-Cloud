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
