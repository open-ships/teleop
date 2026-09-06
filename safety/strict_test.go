package safety_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/safety"
)

func TestDomainNeutralNamesPreserveCompatibility(t *testing.T) {
	command := safety.Command{Name: "player.move", Payload: []float64{0.2, 0.3}}
	var legacy safety.VesselCommand = command
	var current safety.Command = legacy
	_ = safety.ApplyRequest{Intent: legacy}
	_ = safety.AuthorityConfig{EngineeredSafeState: current}
	profile := safety.DefaultStrictConfig(teleop.ButtonBumperRight)
	var old safety.MaritimeConfig = profile
	var strict safety.StrictConfig = old
	if strict != safety.DefaultMaritimeConfig(teleop.ButtonBumperRight) ||
		safety.DefaultStrictLoopWatchdog != safety.DefaultMaritimeLoopWatchdog {
		t.Fatal("compatibility preset differs from strict defaults")
	}
	for _, construct := range []func(safety.StrictConfig) (*safety.Guard, error){safety.NewStrict, safety.NewMaritime} {
		if _, err := construct(strict); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStrictAndMaritimeRejectWeakeningConfigurations(t *testing.T) {
	for name, mutate := range map[string]func(*safety.StrictConfig){
		"command deadline":     func(c *safety.StrictConfig) { c.CommandTimeout = 0 },
		"transport deadline":   func(c *safety.StrictConfig) { c.TransportTimeout = -time.Second },
		"loop deadline":        func(c *safety.StrictConfig) { c.LoopWatchdog = 0 },
		"reactuation":          func(c *safety.StrictConfig) { c.DeadManReactuation = 0 },
		"dead-man":             func(c *safety.StrictConfig) { c.DeadMan = "" },
		"NaN":                  func(c *safety.StrictConfig) { c.ArmStickTolerance = float32(math.NaN()) },
		"infinity":             func(c *safety.StrictConfig) { c.ArmTriggerTolerance = float32(math.Inf(1)) },
		"negative tolerance":   func(c *safety.StrictConfig) { c.ArmStickTolerance = -0.1 },
		"full-range tolerance": func(c *safety.StrictConfig) { c.ArmTriggerTolerance = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			config := safety.DefaultStrictConfig(teleop.ButtonBumperRight)
			mutate(&config)
			for _, construct := range []func(safety.StrictConfig) (*safety.Guard, error){safety.NewStrict, safety.NewMaritime} {
				if _, err := construct(config); !errors.Is(err, safety.ErrInvalidConfiguration) {
					t.Fatalf("weakened profile accepted: %v", err)
				}
			}
		})
	}
}
