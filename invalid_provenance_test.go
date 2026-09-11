package teleop_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/action"
	"github.com/open-ships/teleop/gesture"
	"github.com/open-ships/teleop/testkit"
)

func TestInvalidObservationPropagatesSyntheticProvenance(t *testing.T) {
	fields := map[string]func(*teleop.State, float32){
		"left x":        func(s *teleop.State, v float32) { s.LeftStick.X = v },
		"left y":        func(s *teleop.State, v float32) { s.LeftStick.Y = v },
		"right x":       func(s *teleop.State, v float32) { s.RightStick.X = v },
		"right y":       func(s *teleop.State, v float32) { s.RightStick.Y = v },
		"left trigger":  func(s *teleop.State, v float32) { s.LeftTrigger = v },
		"right trigger": func(s *teleop.State, v float32) { s.RightTrigger = v },
	}
	for name, set := range fields {
		for _, invalid := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -2, 2} {
			t.Run(fmt.Sprintf("%s/%v", name, invalid), func(t *testing.T) {
				source := testkit.NewFakeSource(teleop.Descriptor{}, 8)
				mapper := action.New(action.OnButton("release", teleop.ButtonFaceSouth, teleop.PhaseReleased), action.OnTrigger("trigger", teleop.RightTrigger))
				controller, err := teleop.NewController(source, teleop.WithDeferredStart(), teleop.WithProcessor(gesture.New(gesture.DefaultConfig())), teleop.WithProcessor(mapper))
				if err != nil {
					t.Fatal(err)
				}
				defer controller.Close()
				sub, err := controller.Subscribe(teleop.SubscriptionOptions{Buffer: 128})
				if err != nil {
					t.Fatal(err)
				}
				defer sub.Close()
				held := teleop.State{LeftStick: teleop.Stick{X: 0.8}, RightStick: teleop.Stick{Y: 0.8}, LeftTrigger: 0.8, RightTrigger: 0.8}
				held.SetButton(teleop.ButtonFaceSouth, true)
				if err := source.Push(t.Context(), held); err != nil {
					t.Fatal(err)
				}
				broken := held.Clone()
				set(&broken, float32(invalid))
				if err := source.Push(t.Context(), broken); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				var rejected bool
				var input, gestures, actions int
				for {
					event, err := sub.Next(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if value, ok := event.(teleop.ObservationEvent); ok && value.Meta.Synthetic {
						rejected = true
						continue
					}
					if !rejected {
						if event.Kind() != teleop.EventError && event.Header().Synthetic {
							t.Fatalf("valid input unexpectedly synthetic: %+v", event)
						}
						continue
					}
					if !event.Header().Synthetic {
						t.Fatalf("rejected input produced physical event: %+v", event)
					}
					switch value := event.(type) {
					case teleop.ButtonEvent, teleop.StickEvent, teleop.TriggerEvent:
						input++
					case gesture.Event:
						gestures++
					case action.Event:
						actions++
						if value.Action == "trigger" {
							if input != 5 || gestures < 4 || actions != 2 {
								t.Fatalf("incomplete synthetic transitions: input=%d gesture=%d action=%d", input, gestures, actions)
							}
							return
						}
					}
				}
			})
		}
	}
}
