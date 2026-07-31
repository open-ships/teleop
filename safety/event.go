package safety

import (
	"slices"
	"time"

	"github.com/open-ships/teleop"
)

// EventDecision is the kind of every safety event. Decisions are published
// into the controller's ordinary event stream and audit sinks, so the record
// shows not only what the operator did but what the gate permitted.
const EventDecision teleop.EventKind = "safety.decision"

// State is the gate's authorization state.
type State string

const (
	// StateSafe inhibits output. It is the state of a Guard that has not been
	// armed, has been tripped, or cannot prove its conditions are met.
	StateSafe State = "safe"
	// StateArmed means every automatic condition holds but the operator is not
	// currently engaging the dead-man control.
	StateArmed State = "armed"
	// StateLive permits output.
	StateLive State = "live"
)

// Reason identifies why output is inhibited. A decision may carry several.
type Reason string

const (
	// ReasonUnbound reports a Guard that was never bound to a controller.
	ReasonUnbound Reason = "unbound"
	// ReasonNotArmed reports that no operator has armed the gate, or that a
	// previous trip latched it and it has not been re-armed.
	ReasonNotArmed Reason = "not_armed"
	// ReasonEmergencyStop reports a latched emergency stop.
	ReasonEmergencyStop Reason = "emergency_stop"
	// ReasonCommandTimeout reports that the newest observation is older than
	// the configured command timeout, measured on the monotonic clock.
	ReasonCommandTimeout Reason = "command_timeout"
	// ReasonNoInput reports that no observation has ever arrived, so the input
	// path has never been proven to work.
	ReasonNoInput Reason = "no_input"
	// ReasonDeadManReleased reports that the dead-man control is not held.
	ReasonDeadManReleased Reason = "dead_man_released"
	// ReasonDeadManUnconfirmed reports a control the snapshot shows as held
	// whose press the Guard never observed, so it cannot bound how long the
	// hold has lasted. This is ordinary at startup and while a press is still
	// propagating through the pipeline, so it inhibits without latching and
	// clears on the next observed press.
	ReasonDeadManUnconfirmed Reason = "dead_man_unconfirmed"
	// ReasonDeadManStale reports a dead-man control held continuously past the
	// re-actuation deadline, which is the signature of a defeated switch.
	ReasonDeadManStale Reason = "dead_man_stale"
	// ReasonLoopStalled reports that the application control loop stopped
	// calling Heartbeat within its watchdog interval.
	ReasonLoopStalled Reason = "loop_stalled"
	// ReasonDisconnected reports that the controller is not connected.
	ReasonDisconnected Reason = "disconnected"
	// ReasonSynthetic reports state the controller synthesized rather than
	// observed, such as the neutral state published after a disconnect.
	ReasonSynthetic Reason = "synthetic_state"
	// ReasonControllerFault reports a terminated controller pipeline.
	ReasonControllerFault Reason = "controller_fault"
	// ReasonOperator reports an explicit Disarm.
	ReasonOperator Reason = "operator_disarmed"
)

// Event records a change in authorization. Guards publish one on every
// transition, never on every evaluation, so a steady state does not flood the
// log.
type Event struct {
	Meta   teleop.Header `json:"header"`
	State  State         `json:"state"`
	Permit bool          `json:"permit"`
	// Reasons lists every unmet condition, in a stable order.
	Reasons []Reason `json:"reasons,omitempty"`
	// InputAge is the age of the newest observation on the monotonic clock.
	InputAge time.Duration `json:"input_age"`
	// Detail carries operator-supplied context for a manual transition.
	Detail string `json:"detail,omitempty"`
}

// Header implements teleop.Event.
func (e Event) Header() teleop.Header { return e.Meta.Clone() }

// Kind implements teleop.Event.
func (Event) Kind() teleop.EventKind { return EventDecision }

// CloneEvent implements teleop.EventCloner.
func (e Event) CloneEvent() teleop.Event {
	e.Meta = e.Meta.Clone()
	e.Reasons = slices.Clone(e.Reasons)
	return e
}

// Decision is the authorization answer for one instant. Treat a Decision as
// valid only at the moment it was produced: re-evaluate before every command.
type Decision struct {
	// Permit reports whether output is authorized. It is false whenever the
	// Guard could not affirmatively establish every condition.
	Permit bool
	State  State
	// Reasons is empty when Permit is true.
	Reasons []Reason
	// Command is the controller state to act on: the observed state when
	// permitted, and the neutral zero state when inhibited. Using it removes
	// the chance of acting on live input after an inhibiting decision.
	Command teleop.State
	// InputAge is the age of the newest observation on the monotonic clock.
	InputAge time.Duration
	// EvaluatedAt is the monotonic reading at which this decision was made.
	EvaluatedAt time.Duration
}

// Has reports whether the decision carries a specific reason.
func (d Decision) Has(reason Reason) bool {
	return slices.Contains(d.Reasons, reason)
}
