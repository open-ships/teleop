package eventorder

import (
	"math"
	"testing"
)

func TestHighWaterRetainsOneCounterPerStream(t *testing.T) {
	const events = 1_000_000
	var water HighWater
	for sequence := uint64(1); sequence <= events; sequence++ {
		if !water.Advance("input", sequence) {
			t.Fatalf("Advance(input, %d) rejected", sequence)
		}
	}
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if !water.Advance("command", sequence) {
			t.Fatalf("Advance(command, %d) rejected", sequence)
		}
	}

	if got := water.StreamCount(); got != 2 {
		t.Fatalf("retained streams = %d, want 2 after %d events", got, events+3)
	}
	if !water.Contains("input", 1) || !water.Contains("input", events) {
		t.Fatal("published input prefix is not addressable")
	}
	if water.Contains("input", events+1) || water.Contains("missing", 1) {
		t.Fatal("unpublished sequence reported as present")
	}
}

func TestHighWaterRequiresAContiguousNonzeroPrefix(t *testing.T) {
	var water HighWater
	for _, sequence := range []uint64{0, 2, math.MaxUint64} {
		if water.Advance("input", sequence) {
			t.Fatalf("Advance(input, %d) accepted before sequence 1", sequence)
		}
	}
	if water.Advance("", 1) {
		t.Fatal("empty stream accepted")
	}
	if !water.Advance("input", 1) {
		t.Fatal("first sequence rejected")
	}
	for _, sequence := range []uint64{1, 0, 3} {
		if water.Advance("input", sequence) {
			t.Fatalf("noncontiguous Advance(input, %d) accepted", sequence)
		}
	}
	if !water.Advance("input", 2) {
		t.Fatal("next contiguous sequence rejected")
	}
}

func TestHighWaterRejectsAStreamBeyondItsFiniteLimit(t *testing.T) {
	water := New(2)
	if !water.Advance("input", 1) || !water.Advance("command", 1) {
		t.Fatal("streams within the limit were rejected")
	}
	if water.Expected("third-party") != 0 || water.Advance("third-party", 1) {
		t.Fatal("stream beyond the limit was accepted")
	}
	if water.StreamCount() != 2 {
		t.Fatalf("retained streams = %d, want bounded count 2", water.StreamCount())
	}
	if !water.Advance("input", 2) {
		t.Fatal("existing stream could not advance at the stream limit")
	}
}
