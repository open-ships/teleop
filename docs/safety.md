# Command timeout and dead-man switch

`teleop` transports operator input. It does not decide whether acting on that
input is safe. The `safety` package makes that decision explicit and fails
closed.

> [!IMPORTANT]
> A `safety.Guard` is not a certified safety controller. It does not replace a
> hardware emergency stop, a safety-rated interlock, or an actuator that reaches
> a safe state on its own when commands stop arriving. It is the software half
> of a dead-man policy, and it is only as good as the actuation path that
> honors its decisions.

## The problem it solves

The dangerous failure in a teleoperation system is not a corrupted input value.
It is the absence of input: the operator releases a control and the release
never arrives, or the operator becomes unable to act and the last command keeps
being applied. A backend cannot report what the operating system, driver, or
radio never delivered, so the input stream alone can never distinguish "the
operator is holding steady" from "the link died three seconds ago."

Only elapsed time distinguishes them, and only an output gate can act on it.

## Setup

```go
guard := safety.New(
    safety.WithCommandTimeout(150*time.Millisecond),
    safety.WithDeadMan(xbox.ButtonBumperRight),
    safety.WithDeadManReactuation(30*time.Second),
    safety.WithLoopWatchdog(100*time.Millisecond),
)

controller, err := provider.Open(ctx, deviceID,
    teleop.WithProcessor(guard),
    teleop.WithLiveness(20*time.Millisecond, 100*time.Millisecond),
    teleop.WithAuditSink(recorder),
)
if err != nil {
    return err
}
guard.Bind(controller)
```

Attaching the guard with `teleop.WithProcessor` gives it lossless edge
detection on the dead-man control and publishes every transition into the
event stream and audit sinks. `Bind` is separate because the guard must exist
before `Open` and the controller does not exist until after it.

Pair a short command timeout with `teleop.WithLiveness`. Change-driven backends
emit nothing while a control is held steady, so without periodic observations a
held control will trip the timeout.

## The control loop

```go
for range ticker.C {
    guard.Heartbeat()

    decision := guard.Evaluate()
    drive(decision.Command) // neutral whenever inhibited

    _ = controller.RecordCommand(ctx, teleop.Command{
        Name:       "thrust.set",
        Payload:    thrust,
        Authorized: decision.Permit,
        Reason:     firstReason(decision),
    })
}
```

Three properties of this loop matter:

`Evaluate` is authoritative and must be called immediately before acting, every
time. A decision describes the instant it was made and nothing after it.

`Decision.Command` is the observed state when permitted and the neutral zero
state when inhibited. Using it instead of a separate snapshot removes the
possibility of acting on live input after an inhibiting decision.

`Evaluate` is not read-only. A failed condition latches the guard, so authority
stays revoked until an operator re-arms.

## Conditions

A guard authorizes output only when it can affirmatively establish all of:

| Condition | Inhibits when | Reason |
| --- | --- | --- |
| Bound | `Bind` was never called | `ReasonUnbound` |
| Armed | never armed, or latched by a trip | `ReasonNotArmed` |
| Input exists | no observation has ever arrived | `ReasonNoInput` |
| Input fresh | newest observation older than the timeout | `ReasonCommandTimeout` |
| Connected | controller reports disconnected | `ReasonDisconnected` |
| Observed | state was synthesized, not observed | `ReasonSynthetic` |
| Pipeline healthy | controller terminated | `ReasonControllerFault` |
| Loop alive | no `Heartbeat` within the watchdog | `ReasonLoopStalled` |
| Operator engaged | dead-man control not held | `ReasonDeadManReleased` |
| Switch not defeated | held past the re-actuation deadline | `ReasonDeadManStale` |
| No stop | emergency stop active | `ReasonEmergencyStop` |

Anything the guard cannot prove inhibits output. A guard that has never been
bound, never been armed, or never seen an observation denies.

## Design decisions worth knowing

**Freshness is measured on the monotonic clock.** `StateMeta.ReceivedMonotonic`
and `Controller.Monotonic` come from a clock no NTP step or manual adjustment
can move. Enforcing a timeout against wall-clock fields would let a backward
clock step make stale input appear fresh.

**A never-observed input path is treated as failed, not quiet.** Silence that
has never been broken is not evidence that anything works.

**Trips latch by default.** Automatic resumption after a fault is how a
transient dropout becomes an unexpected movement. Clearing a trip requires
`Arm`. `WithLatching(false)` is available and is rarely the right choice.

**Releasing the dead-man control does not latch.** Letting go is normal
operation, so re-engaging restores authority without a re-arm. Every other
condition is a fault and latches. The distinction is what keeps the switch
usable without making faults self-clearing.

**An indefinitely held dead-man control is treated as defeated.** A control
held past `WithDeadManReactuation` is indistinguishable from one taped down,
wedged, or held by an incapacitated operator. The default deadline is one
minute. A guard bound while the control was already down cannot bound the hold
and reports `ReasonDeadManStale` until it observes a fresh press.

**An emergency stop cannot be cleared by `Arm`.** It requires `Reset` first, so
a stop is not undone by reflex.

**The guard trips on its own timer.** As a `teleop.AdvancingProcessor` it is
re-evaluated on the controller's internal ticker, so a command timeout or a
stalled control loop revokes authority and records the transition even if the
application stops calling `Evaluate`. Ticker granularity is roughly 25 ms, so
the application's own `Evaluate` call remains the precise enforcement point.

## Recording

Guard transitions are published as `safety.Event` values into the same
subscriptions and audit sinks as input, so the record shows not only what the
operator did but what the gate permitted. Transitions are emitted on change
only, so a steady state costs nothing.

Record inhibited commands as well as authorized ones. A recorded unauthorized
command shows that the application computed one and the gate stopped it, which
is exactly the evidence that the gate worked.

`RecordCommand` never blocks and returns `ErrPipelineOverflow` when its queue
is full. Treat that as a fault and inhibit: the command just issued is not in
the record.

## What this does not cover

The guard gates commands leaving the application. It cannot ensure the actuator
honors them, cannot detect a control surface that fails in place, and cannot
substitute for an actuator-side timeout. A remote system should reach a safe
state on its own when commands stop arriving, independently of anything decided
here.
