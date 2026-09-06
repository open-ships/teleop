package assured_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/open-ships/teleop/assured"
	"github.com/open-ships/teleop/safety"
)

func TestGenericAssuredConfigRejectsAmbiguityAndMissingGuarantees(t *testing.T) {
	for name, mutate := range map[string]func(*assured.Config){
		"neither profile": func(c *assured.Config) { c.Safety = safety.StrictConfig{} },
		"both identical":  func(c *assured.Config) { c.Maritime = c.Safety },
		"both conflicting": func(c *assured.Config) {
			c.Maritime = c.Safety
			c.Maritime.TransportTimeout *= 2
		},
		"partial profiles not merged": func(c *assured.Config) {
			c.Maritime.DeadMan, c.Safety.DeadMan = c.Safety.DeadMan, ""
		},
		"missing dead-man":                func(c *assured.Config) { c.Safety.DeadMan = "" },
		"missing transport deadline":      func(c *assured.Config) { c.Safety.TransportTimeout = 0 },
		"invalid tolerance":               func(c *assured.Config) { c.Safety.ArmStickTolerance = float32(math.NaN()) },
		"lease exceeds command timeout":   func(c *assured.Config) { c.Safety.CommandTimeout = time.Millisecond },
		"lease exceeds transport timeout": func(c *assured.Config) { c.Safety.TransportTimeout = time.Millisecond },
		"lease exceeds watchdog":          func(c *assured.Config) { c.Safety.LoopWatchdog = time.Millisecond },
		"lease exceeds reactuation":       func(c *assured.Config) { c.Safety.DeadManReactuation = time.Millisecond },
		"policy required":                 func(c *assured.Config) { c.Authority.Policy = nil },
		"ack required":                    func(c *assured.Config) { c.Authority.RequireAppliedAcknowledgment = false },
		"signer required":                 func(c *assured.Config) { c.Signer = nil },
		"witness required":                func(c *assured.Config) { c.Anchor = nil },
		"store required":                  func(c *assured.Config) { c.EvidenceStore = nil },
		"actuator required":               func(c *assured.Config) { c.Actuator = nil },
		"system clock required":           func(c *assured.Config) { c.Authority.Now = time.Now },
	} {
		t.Run(name, func(t *testing.T) {
			store, anchor, actuator := &syncStore{}, &witnessAnchor{}, &acceptingActuator{}
			config, _ := validConfig(t, store, anchor, actuator)
			config.Safety, config.Maritime = config.Maritime, safety.MaritimeConfig{}
			mutate(&config)
			source := exactSource(false)
			defer source.Close()
			session, err := assured.OpenSource(t.Context(), source, config)
			if session != nil {
				_ = session.Close()
				t.Fatal("invalid configuration opened a session")
			}
			if !errors.Is(err, assured.ErrInvalidConfig) {
				t.Fatalf("expected invalid configuration: %v", err)
			}
			if len(store.bytes()) != 0 || len(actuator.snapshot()) != 0 {
				t.Fatal("invalid configuration reached the journal or actuator")
			}
		})
	}
}
