package testkit_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/testkit"
)

func observationWithReferences() teleop.Observation {
	state := teleop.State{}
	state.SetButton("extension.test", true)
	return teleop.Observation{State: state, ObservedAt: time.Now(), Native: teleop.NativeInput{Data: []byte{1}, Fields: map[string]int64{"value": 1}}, Gap: &teleop.SourceGap{Dropped: 1, Reason: "original"}}
}

func mutateObservation(value teleop.Observation) {
	value.State.SetButton("extension.test", false)
	value.Native.Data[0] = 9
	value.Native.Fields["value"] = 9
	value.Gap.Dropped, value.Gap.Reason = 9, "changed"
}

func TestReplayOwnsNestedObservationsAndIsolatesReaders(t *testing.T) {
	original := observationWithReferences()
	want := original.Clone()
	first := testkit.NewReplaySource(teleop.Descriptor{}, []teleop.Observation{original, original})
	second := testkit.NewReplaySource(teleop.Descriptor{}, []teleop.Observation{original})
	mutateObservation(original)
	got, err := first.Read(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("constructor retained caller state: %+v %v", got, err)
	}
	mutateObservation(got)
	for _, source := range []*testkit.ReplaySource{first, second} {
		got, err := source.Read(t.Context())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("shared replay data: %+v %v", got, err)
		}
	}
}

func TestFakeSourceCopiesAtPush(t *testing.T) {
	original := observationWithReferences()
	want := original.Clone()
	source := testkit.NewFakeSource(teleop.Descriptor{}, 1)
	defer source.Close()
	if err := source.PushObservation(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	mutateObservation(original)
	got, err := source.Read(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("push retained caller state: %+v %v", got, err)
	}
}
