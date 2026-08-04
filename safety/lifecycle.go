package safety

import (
	"context"
	"errors"

	"github.com/open-ships/statemachine"
)

// lifecycle is the operator's standing intent for the gate, independent of
// whether the automatic conditions currently hold. Evaluate derives the
// published State from this together with the live conditions, so this type
// records only what a command established: nothing an input observation does
// can change it, and nothing but a command can restore authority once it is
// lost.
type lifecycle string

const (
	// lifecycleIdle inhibits output. It is the state of a gate that has never
	// been armed, that an operator disarmed, or that a fault tripped.
	lifecycleIdle lifecycle = "idle"
	// lifecycleArmed means an operator authorized output, subject to every
	// configured condition still being provable at the instant of each
	// Evaluate.
	lifecycleArmed lifecycle = "armed"
	// lifecycleStopped is a latched emergency stop. Only Reset clears it, and
	// clearing it does not restore authority.
	lifecycleStopped lifecycle = "stopped"
)

// command is an operator action, or the internal trip, that moves the
// lifecycle. Input events are not commands: they are conditions Evaluate
// re-derives, not transitions.
type command string

const (
	commandArm    command = "arm"
	commandDisarm command = "disarm"
	commandTrip   command = "trip"
	commandStop   command = "stop"
	commandReset  command = "reset"
)

// errArmDuringStop is the refusal reason carried by the arm row that a latched
// emergency stop declines.
var errArmDuringStop = errors.New("teleop/safety: cannot arm during an emergency stop")

// transition is an alias, not a defined type, so MustCompile can infer the
// machine's type parameters from the table.
type transition = statemachine.Transition[lifecycle, command, *Guard]

// gate is the operator-command state machine. It is the whole answer to which
// commands are legal in which state; the booleans this table replaced encoded
// the same rules implicitly, and had to be kept mutually consistent by hand at
// every assignment.
//
// The machine holds no state. The current lifecycle lives on the Guard, under
// the Guard's own mutex, which also protects everything else a transition
// touches.
var gate = statemachine.MustCompile([]transition{
	// Arming requires a bound controller, and is refused outright while a stop
	// is latched: clearing one has to be a deliberate Reset rather than a
	// reflexive re-arm. The stopped row exists to carry that reason; its Guard
	// never applies, so it is never selected.
	{From: lifecycleIdle, Event: commandArm, To: lifecycleArmed, Guard: requireBound},
	{From: lifecycleArmed, Event: commandArm, To: lifecycleArmed, Guard: requireBound},
	{From: lifecycleStopped, Event: commandArm, To: lifecycleArmed, Guard: refuseArmDuringStop},

	// Disarming and tripping both drop authority. Neither clears a stop, so
	// both are self-transitions once one is latched.
	{From: lifecycleIdle, Event: commandDisarm, To: lifecycleIdle},
	{From: lifecycleArmed, Event: commandDisarm, To: lifecycleIdle},
	{From: lifecycleStopped, Event: commandDisarm, To: lifecycleStopped},

	// Only an armed gate can trip; Evaluate fires this when a condition it
	// cannot prove latches the gate.
	{From: lifecycleArmed, Event: commandTrip, To: lifecycleIdle},

	// A stop latches from every state, including itself.
	{From: lifecycleIdle, Event: commandStop, To: lifecycleStopped},
	{From: lifecycleArmed, Event: commandStop, To: lifecycleStopped},
	{From: lifecycleStopped, Event: commandStop, To: lifecycleStopped},

	// Reset always leaves output inhibited until the next Arm. Making Reset from
	// an armed state idle avoids a reset request unexpectedly preserving live
	// authority.
	{From: lifecycleStopped, Event: commandReset, To: lifecycleIdle},
	{From: lifecycleIdle, Event: commandReset, To: lifecycleIdle},
	{From: lifecycleArmed, Event: commandReset, To: lifecycleIdle},
})

// requireBound declines while the Guard has no controller. Binding is checked
// before any other arming condition so an unbound gate reports that first.
func requireBound(_ context.Context, g *Guard) error {
	if g.source == nil {
		return ErrUnbound
	}
	return nil
}

// refuseArmDuringStop declines every arm attempt made while a stop is latched,
// reporting the unbound gate first so the reason does not depend on which
// condition is checked first.
func refuseArmDuringStop(ctx context.Context, g *Guard) error {
	if err := requireBound(ctx, g); err != nil {
		return err
	}
	return errArmDuringStop
}

// fire applies an internal or inhibiting command. It preserves the current
// state if the transition table refuses the command, and clears every piece of
// authorization proof after any accepted authority-revoking transition. The
// caller must hold g.mu.
func (g *Guard) fire(event command) {
	next, err := gate.Fire(context.Background(), g.lifecycle, event, g)
	if err != nil {
		return
	}
	g.lifecycle = next
	switch event {
	case commandDisarm, commandTrip, commandStop, commandReset:
		g.clearAuthorizationProofLocked()
	}
}
