package action_test

import (
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/gesture"
)

func TestMapperPreservesCausality(t *testing.T) {
	t.Parallel()

	mapper := action.New(
		action.OnButton("confirm", teleop.ButtonFaceSouth, teleop.PhasePressed),
		action.OnGesture("arm", gesture.Hold, teleop.ButtonBumperLeft),
	)
	source := teleop.ButtonEvent{
		Meta: teleop.Header{
			ID:         teleop.EventID{Stream: "input", Sequence: 42},
			ObservedAt: time.Now(),
		},
		Button:  teleop.ButtonFaceSouth,
		Phase:   teleop.PhasePressed,
		Pressed: true,
	}
	result := mapper.Map(source)
	if len(result) != 1 || result[0].Action != "confirm" {
		t.Fatalf("actions = %#v", result)
	}
	if len(result[0].Meta.Causes) != 1 || result[0].Meta.Causes[0] != source.Meta.ID {
		t.Fatalf("causes = %#v", result[0].Meta.Causes)
	}
}
