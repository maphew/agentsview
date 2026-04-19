package db

import (
	"math"
	"testing"
)

func TestAssignBucketDurationEdges(t *testing.T) {
	cases := []struct {
		v    float64
		want int
	}{
		{0, 0}, {0.5, 0}, {1, 1}, {4.999, 1}, {5, 2},
		{19.999, 2}, {20, 3}, {59.999, 3}, {60, 4},
		{120, 5}, {120.1, 5}, {9999, 5},
	}
	for _, c := range cases {
		got := assignBucket(durationMinutesEdges, c.v)
		if got != c.want {
			t.Errorf("durationMinutes v=%v: got %d, want %d", c.v, got, c.want)
		}
	}
}

func TestAssignBucketUserMessagesAll(t *testing.T) {
	cases := []struct {
		v    float64
		want int // index into userMessagesEdgesAll (7 edges → 6 buckets)
	}{
		{0, 0}, {1, 0}, {1.9, 0}, // scope_all bucket [0,2)
		{2, 1}, {5, 1}, {5.9, 1}, // [2,6)
		{6, 2}, {15.9, 2}, // [6,16)
		{16, 3}, {30.9, 3},
		{31, 4}, {50.9, 4},
		{51, 5}, {10000, 5},
	}
	for _, c := range cases {
		got := assignBucket(userMessagesEdgesAll, c.v)
		if got != c.want {
			t.Errorf("user_messages scope_all v=%v: got %d, want %d", c.v, got, c.want)
		}
	}
}

func TestBuildEmptyBucketsTopIsUnbounded(t *testing.T) {
	b := buildEmptyBuckets(durationMinutesEdges)
	if len(b) != 6 {
		t.Fatalf("want 6 buckets, got %d", len(b))
	}
	top := b[len(b)-1]
	if top.Edge[1] != nil {
		t.Errorf("top bucket hi should be nil (JSON null), got %v", *top.Edge[1])
	}
	if math.IsInf(*top.Edge[0], 1) {
		t.Errorf("top bucket lo should be finite, got +Inf")
	}
}

// TestEdgeListsShape pins every v1 edge list to the spec's bucket counts
// and guards against accidental reordering. Each entry also catches the
// unused-linter on edge lists whose first consumer lives in a later task.
func TestEdgeListsShape(t *testing.T) {
	cases := []struct {
		name        string
		edges       []float64
		wantBuckets int
		wantTopInf  bool
	}{
		{"durationMinutes", durationMinutesEdges, 6, true},
		{"userMessagesAll", userMessagesEdgesAll, 6, true},
		{"userMessagesHuman", userMessagesEdgesHuman, 5, true},
		{"peakContext", peakContextEdges, 6, true},
		{"toolsPerTurn", toolsPerTurnEdges, 6, true},
		{"cacheHitRatio", cacheHitRatioEdges, 5, false}, // inclusive of 1.0 via 1.000001
	}
	for _, c := range cases {
		if got := len(c.edges) - 1; got != c.wantBuckets {
			t.Errorf("%s: want %d buckets, got %d", c.name, c.wantBuckets, got)
		}
		for i := 1; i < len(c.edges); i++ {
			if !(c.edges[i] > c.edges[i-1]) {
				t.Errorf("%s: edges must be strictly increasing; edges[%d]=%v <= edges[%d]=%v",
					c.name, i, c.edges[i], i-1, c.edges[i-1])
			}
		}
		topIsInf := math.IsInf(c.edges[len(c.edges)-1], 1)
		if topIsInf != c.wantTopInf {
			t.Errorf("%s: top edge +Inf = %v, want %v", c.name, topIsInf, c.wantTopInf)
		}
	}
}
