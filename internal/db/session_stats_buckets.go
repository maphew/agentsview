package db

import "math"

// Schema v1 bucket boundaries. Changing any of these requires a
// schema_version bump (see session-analytics-design spec).

// Half-open intervals [lo, hi). The last bucket has hi = +Inf.
var durationMinutesEdges = []float64{0, 1, 5, 20, 60, 120, math.Inf(1)}

// user_messages scope_all: [0,1), [1,2), [2,5], [6,15], [16,30], [31,50], [51,inf)
// user_messages scope_human: [2,5], [6,15], [16,30], [31,50], [51,inf) -- no automation buckets
// Represented as two separate edge lists for clarity.
var userMessagesEdgesAll = []float64{0, 2, 6, 16, 31, 51, math.Inf(1)}
var userMessagesEdgesHuman = []float64{2, 6, 16, 31, 51, math.Inf(1)}

var peakContextEdges = []float64{0, 10_000, 50_000, 100_000, 150_000, 200_000, math.Inf(1)}
var toolsPerTurnEdges = []float64{0, 1, 2, 4, 7, 11, math.Inf(1)}
var cacheHitRatioEdges = []float64{0, 0.25, 0.5, 0.75, 0.95, 1.000001} // inclusive of 1.0

// assignBucket returns the index i such that edges[i] <= v < edges[i+1],
// or -1 if v < edges[0] or v >= edges[len-1] (shouldn't happen given Inf upper).
func assignBucket(edges []float64, v float64) int {
	for i := 0; i < len(edges)-1; i++ {
		if v >= edges[i] && v < edges[i+1] {
			return i
		}
	}
	return -1
}

// buildEmptyBuckets returns a pre-sized bucket slice matching edges[i]..edges[i+1].
// Top bucket's hi is represented as JSON null by leaving Edge[1] as nil pointer.
func buildEmptyBuckets(edges []float64) []DistributionBucketV1 {
	out := make([]DistributionBucketV1, 0, len(edges)-1)
	for i := 0; i < len(edges)-1; i++ {
		lo := edges[i]
		var hiPtr *float64
		if !math.IsInf(edges[i+1], 1) {
			hi := edges[i+1]
			hiPtr = &hi
		}
		loPtr := lo
		out = append(out, DistributionBucketV1{
			Edge:  [2]*float64{&loPtr, hiPtr},
			Count: 0,
		})
	}
	return out
}
