// Package eventorder tracks the contiguous prefix of events published in each
// stream. It deliberately stores one counter per stream rather than one entry
// per event so long-running sessions retain bounded causal-index state.
package eventorder

import "math"

// DefaultMaxStreams is the fixed production ceiling for distinct event streams
// in one session. It is intentionally generous for application-defined streams
// while preventing an adversarial stream-per-event workload from recreating an
// O(events) identity index.
const DefaultMaxStreams = 4096

// HighWater records the greatest contiguous published sequence in each stream.
// The zero value is ready for use. Callers must provide synchronization when a
// HighWater is shared between goroutines.
type HighWater struct {
	streams    map[string]uint64
	maxStreams int
}

// New returns an empty high-water index with a finite stream ceiling. Values
// less than one select DefaultMaxStreams.
func New(maxStreams int) HighWater {
	if maxStreams < 1 {
		maxStreams = DefaultMaxStreams
	}
	return HighWater{maxStreams: maxStreams}
}

// Expected returns the only sequence that can extend stream's contiguous
// published prefix. A zero result means stream is invalid, the index is at its
// distinct-stream limit, or the stream is exhausted at MaxUint64.
func (water *HighWater) Expected(stream string) uint64 {
	if stream == "" {
		return 0
	}
	var current uint64
	var exists bool
	if water != nil {
		current, exists = water.streams[stream]
	}
	if !exists && water.StreamCount() >= water.Limit() {
		return 0
	}
	if current == math.MaxUint64 {
		return 0
	}
	return current + 1
}

// Advance extends stream's contiguous published prefix. It rejects empty
// streams, zero sequences, gaps, duplicates, and regressions.
func (water *HighWater) Advance(stream string, sequence uint64) bool {
	if stream == "" || sequence == 0 || sequence != water.Expected(stream) {
		return false
	}
	if water.streams == nil {
		water.streams = make(map[string]uint64)
	}
	water.streams[stream] = sequence
	return true
}

// Contains reports whether the identity is in stream's contiguous published
// prefix. Session identity remains the caller's responsibility.
func (water *HighWater) Contains(stream string, sequence uint64) bool {
	return stream != "" && sequence > 0 && sequence <= water.Through(stream)
}

// Through returns the greatest contiguous sequence published in stream.
func (water *HighWater) Through(stream string) uint64 {
	if water == nil {
		return 0
	}
	return water.streams[stream]
}

// StreamCount returns the amount of retained index state.
func (water *HighWater) StreamCount() int {
	if water == nil {
		return 0
	}
	return len(water.streams)
}

// Limit returns the maximum distinct streams retained by this index.
func (water *HighWater) Limit() int {
	if water == nil || water.maxStreams < 1 {
		return DefaultMaxStreams
	}
	return water.maxStreams
}
