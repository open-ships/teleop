package safety_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/safety"
)

// gameAdapter is an in-process TEST model, contrasting with the signed scalar
// vehicle receiver exercised in assured/policy_integration_test.go. Advance,
// not a goroutine or physical watchdog, drives its lease expiry.
type gameAdapter struct {
	mu      sync.Mutex
	now     time.Time
	last    safety.ActuatorCommand
	paused  bool
	move    [2]float64
	buttons []string
}

var _ safety.Actuator = (*gameAdapter)(nil)

func (game *gameAdapter) Now() time.Time {
	game.mu.Lock()
	defer game.mu.Unlock()
	return game.now
}

func (game *gameAdapter) Advance(elapsed time.Duration) error {
	game.mu.Lock()
	defer game.mu.Unlock()
	if elapsed < 0 {
		return errors.New("game model cannot reverse time")
	}
	game.now = game.now.Add(elapsed)
	if !game.now.Before(game.last.ExpiresAt) {
		game.paused, game.move, game.buttons = true, [2]float64{}, nil
	}
	return nil
}

func (game *gameAdapter) snapshot() (safety.ActuatorCommand, bool, [2]float64, []string) {
	game.mu.Lock()
	defer game.mu.Unlock()
	return game.last.Clone(), game.paused, game.move, slices.Clone(game.buttons)
}

func (game *gameAdapter) Send(ctx context.Context, command safety.ActuatorCommand) (safety.ActuatorReceipt, error) {
	game.mu.Lock()
	defer game.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if command.ControllerSession == (teleop.SessionID{}) ||
		(game.last.Sequence != 0 && command.ControllerSession != game.last.ControllerSession) ||
		command.Sequence <= game.last.Sequence || command.TTL <= 0 ||
		command.IssuedAt.After(game.now) || !command.ExpiresAt.After(game.now) ||
		command.ExpiresAt.Sub(game.now) > command.TTL {
		return nil, errors.New("game model rejected session, ordering or lease")
	}
	var input struct {
		Player  string    `json:"player"`
		Move    []float64 `json:"move"`
		Buttons []string  `json:"buttons"`
	}
	if command.Fallback {
		if command.Command.Name != "game.pause" || len(command.Command.Payload) != 0 {
			return nil, errors.New("game model rejected fallback")
		}
	} else {
		decoder := json.NewDecoder(bytes.NewReader(command.Command.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return nil, err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, errors.New("game model rejected trailing JSON")
		}
		if command.Command.Name != "game.move" || input.Player != "player-7" || len(input.Move) != 2 ||
			math.Abs(input.Move[0]) > 1 || math.Abs(input.Move[1]) > 1 {
			return nil, errors.New("game model rejected movement")
		}
	}
	game.last = command.Clone()
	game.paused, game.move, game.buttons = command.Fallback, [2]float64{}, input.Buttons
	copy(game.move[:], input.Move)
	ack := safety.ActuatorAcknowledgment{
		ControllerSession: command.ControllerSession, Sequence: command.Sequence,
		Accepted: true, AppliedAt: game.now, Detail: "applied to game model only",
	}
	return safety.ReceiptFunc(func(ctx context.Context) (safety.ActuatorAcknowledgment, error) {
		return ack, ctx.Err()
	}), nil
}

func TestStrictAuthoritySupportsGameShapedCommands(t *testing.T) {
	const deadMan = teleop.ButtonBumperRight
	profile := safety.DefaultStrictConfig(deadMan)
	profile.CommandTimeout, profile.TransportTimeout, profile.LoopWatchdog = time.Second, time.Second, time.Second
	guard, err := safety.NewStrict(profile)
	if err != nil {
		t.Fatal(err)
	}
	game := &gameAdapter{now: time.Now(), paused: true}
	evidence := &memoryEvidence{} // Test double, not a production durability store.
	actuator := safety.ActuatorFunc(func(ctx context.Context, command safety.ActuatorCommand) (safety.ActuatorReceipt, error) {
		records := evidence.snapshot()
		if len(records) == 0 || records[len(records)-1].Kind != safety.EvidenceIntent ||
			records[len(records)-1].Applied == nil ||
			!bytes.Equal(records[len(records)-1].Applied.Payload, command.Command.Payload) {
			t.Error("send preceded exact intent evidence")
		}
		return game.Send(ctx, command)
	})
	authority, err := safety.NewAuthorityWithGuard(safety.AuthorityConfig{
		EngineeredSafeState: safety.Command{Name: "game.pause"},
		CommandTTL:          100 * time.Millisecond, RequireAppliedAcknowledgment: true,
		Now: game.Now, // Deterministic low-level seam, not allowed by assured.Session.
	}, guard, evidence, actuator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close(context.Background()) })
	source := newAuthorityTestSource()
	source.caps = teleop.Capabilities{AuditGrade: teleop.AuditExactBackendStream,
		Controls: []teleop.ControlDescriptor{{ID: deadMan, Kind: teleop.ControlButton}}}
	if err := authority.Bind(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if initial, paused, _, _ := game.snapshot(); !paused || !initial.Fallback {
		t.Fatal("bind did not establish paused baseline")
	}
	if err := authority.Arm(t.Context(), "player ready"); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	source.now += time.Millisecond
	source.mu.Unlock()
	state := teleop.State{}
	state.SetButton(deadMan, true)
	source.setState(state)
	_, meta := source.SnapshotWithMeta()
	header := teleop.Header{ID: teleop.EventID{Session: source.Session(), Stream: "input", Sequence: meta.Sequence}, ReceivedMonotonic: meta.ReceivedMonotonic}
	authority.Processor().Process(teleop.ObservationEvent{Meta: header, Current: state})
	authority.Processor().Process(teleop.ButtonEvent{Meta: header, Button: deadMan, Pressed: true, Phase: teleop.PhasePressed})

	movement, buttons := []float64{0.25, -0.5}, []string{"sprint"}
	payload := map[string]any{"player": "player-7", "move": movement, "buttons": buttons}
	want, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result, err := authority.Apply(t.Context(), safety.ApplyRequest{
		Intent: safety.Command{Name: "game.move", Payload: payload}, Causes: []teleop.EventID{header.ID},
	})
	if err != nil || result.Fallback || !result.Decision.Permit {
		t.Fatalf("game movement: %+v, %v", result, err)
	}
	movement[0], buttons[0] = 1, "mutated"
	command, paused, move, appliedButtons := game.snapshot()
	if paused || move != [2]float64{0.25, -0.5} || !slices.Equal(appliedButtons, []string{"sprint"}) || !bytes.Equal(command.Command.Payload, want) {
		t.Fatalf("game state/bytes changed: %+v %t %v %v", command, paused, move, appliedButtons)
	}
	if result.Acknowledgment == nil || !result.Acknowledgment.Accepted ||
		result.Acknowledgment.Sequence != command.Sequence || result.Acknowledgment.ControllerSession != source.Session() {
		t.Fatalf("acknowledgment does not identify applied game command: %+v", result)
	}
	records := evidence.snapshot()
	chain := records[len(records)-4:]
	for i, kind := range []safety.EvidenceKind{safety.EvidenceDecision, safety.EvidenceIntent, safety.EvidenceSent, safety.EvidenceAcknowledged} {
		if chain[i].Kind != kind || !slices.Contains(chain[i].Causes, header.ID) {
			t.Fatalf("missing game evidence/causality: %+v", chain[i])
		}
	}
	if chain[1].ParentID != result.DecisionEvidenceID || chain[2].ParentID != result.IntentEvidenceID ||
		chain[1].Applied == nil || !bytes.Equal(chain[1].Applied.Payload, want) ||
		chain[3].Acknowledgment == nil || chain[3].Acknowledgment.Sequence != command.Sequence {
		t.Fatalf("game evidence differs from receiver: %+v", chain)
	}
	for name, mutate := range map[string]func(*safety.ActuatorCommand){
		"duplicate":       func(c *safety.ActuatorCommand) {},
		"older":           func(c *safety.ActuatorCommand) { c.Sequence-- },
		"foreign session": func(c *safety.ActuatorCommand) { c.ControllerSession[0] ^= 1; c.Sequence++ },
		"expired":         func(c *safety.ActuatorCommand) { c.Sequence++; c.ExpiresAt = game.Now() },
		"invalid movement": func(c *safety.ActuatorCommand) {
			c.Sequence++
			c.Command.Payload = json.RawMessage(`{"player":"player-7","move":[10,0]}`)
		},
		"wrong vector length": func(c *safety.ActuatorCommand) {
			c.Sequence++
			c.Command.Payload = json.RawMessage(`{"player":"player-7","move":[0]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := command.Clone()
			mutate(&invalid)
			if _, err := game.Send(t.Context(), invalid); err == nil {
				t.Fatal("invalid command accepted")
			}
			if current, paused, move, _ := game.snapshot(); paused || move != [2]float64{0.25, -0.5} || current.Sequence != command.Sequence {
				t.Fatal("rejected command changed game state")
			}
		})
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	fresh := command.Clone()
	fresh.Sequence++
	if _, err := game.Send(cancelled, fresh); !errors.Is(err, context.Canceled) {
		t.Fatalf("adapter ignored cancellation: %v", err)
	}
	// Receiver expiry works with no further controller call or sender packet.
	if err := game.Advance(command.TTL); err != nil {
		t.Fatal(err)
	}
	if _, paused, move, buttons := game.snapshot(); !paused || move != [2]float64{} || len(buttons) != 0 {
		t.Fatal("expired movement remained active")
	}
	if _, err := game.Send(t.Context(), command); err == nil {
		t.Fatal("replayed movement revived expired output")
	}
	if err := authority.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if final, paused, _, _ := game.snapshot(); !paused || !final.Fallback || final.Sequence <= command.Sequence {
		t.Fatal("close did not acknowledge a newer pause command")
	}
}
