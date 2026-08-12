package files

import "testing"

// The centroid update, tested directly.
//
// This is arithmetic, and a behavioural test through the clusterer cannot pin it
// down: whether a wrong centroid changes any assignment depends on the vectors
// the test happens to choose and on where they sit relative to the threshold, so
// such a test passes under both formulas for most inputs and proves nothing.
// The property is exact, so assert it exactly.

func TestWeightedMeanMovesByOneOverNPlusOne(t *testing.T) {
	// A cluster of ten faces sitting at 0, and one new face at 1. The correct
	// mean of eleven such values is 1/11; a midpoint would give 1/2, which is
	// five and a half times too far.
	got := weightedMean([]float32{0, 0}, 10, []float32{1, 1})
	want := float32(1) / 11

	for i, v := range got {
		if diff := v - want; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("component %d = %v, want %v (a midpoint would be 0.5)", i, v, want)
		}
	}
}

// Folding faces in one at a time has to give the same answer as averaging them
// all at once, or a cluster's centroid depends on the order its photos were
// uploaded rather than on who is in them.
func TestWeightedMeanEqualsTheBatchMean(t *testing.T) {
	values := []float32{0.2, 0.9, 0.4, 0.75, 0.1, 0.6}

	mean := []float32{values[0]}
	for i := 1; i < len(values); i++ {
		mean = weightedMean(mean, i, []float32{values[i]})
	}

	var sum float32
	for _, v := range values {
		sum += v
	}
	want := sum / float32(len(values))

	if diff := mean[0] - want; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("incremental mean = %v, batch mean = %v", mean[0], want)
	}
}

// A single-member cluster is the one case where a midpoint was right, so it has
// to stay right.
func TestWeightedMeanOfOneIsAMidpoint(t *testing.T) {
	got := weightedMean([]float32{0}, 1, []float32{1})
	if diff := got[0] - 0.5; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("mean of one and one more = %v, want 0.5", got[0])
	}
}

// Vectors of different lengths cannot legitimately meet — dim is fixed per
// model — but the fold must not panic if they ever do.
func TestWeightedMeanIgnoresExtraComponents(t *testing.T) {
	got := weightedMean([]float32{1, 1, 1}, 1, []float32{0})
	if len(got) != 3 {
		t.Fatalf("length = %d, want 3", len(got))
	}
	if got[1] != 1 || got[2] != 1 {
		t.Errorf("components beyond the shorter vector were altered: %v", got)
	}
}
