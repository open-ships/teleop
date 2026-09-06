# Architecture

Teleop separates controller-independent application code from controller- and
operating-system-specific input and feedback output.

Controller input and `audit` work without actuation. `safety` and `assured`
are optional modules; `assured` retains a strict contract rather than selecting
weaker behavior based on whether an adapter controls a boat, car, or game.
See [integration choices and compatibility](integration.md).

```text
OS controller API
    ↕
xbox device source
    ↕
teleop.Controller
    ├── immutable CanonicalEventSink → audit.Recorder → external witness
    ├── lossless subscription
    ├── latest-value subscription
    └── optional safety.Authority → leased Actuator Command → acknowledgement
              ↑
        optional assured.Session owns strict composition and shutdown
```

## Package boundaries

- `teleop` owns controller-neutral controls, states, rumble output, events,
  immutable sink admission, subscriptions, provider interfaces, normalization,
  transport-health metadata, and lifecycle semantics.
- `xbox` supplies Xbox labels and selects the platform backend.
- `gesture` derives temporal patterns without hiding canonical input.
- `action` maps physical or gesture events to application-defined identifiers.
- `safety` owns strict interlocks, the operator lifecycle state machine, Safety
  Authority, expiring actuator leases, acknowledgement, and safe fallback.
- `audit` stores and verifies the event, command, and causality chain; signs
  tree heads and publishes witness checkpoints.
- `assured` composes one controller, strict Guard, Safety Authority, durable
  recorder, signer, witness, actuator, readiness policy, and ordered shutdown.
- `testkit` supplies deterministic fake and replay sources.

Attach recognizers and mappers with `teleop.WithProcessor`. Processors run in
option order, and their output is published to the same subscribers and audit
sinks as canonical input.

The core module does not pair devices. Bluetooth, USB, and Xbox Wireless
connections are managed by the host operating system.

## Event guarantee

Every observation delivered by an `InputSource` is accepted into each bounded
authoritative-sink queue before it is published to subscriptions or committed
to `Snapshot`. For a `CanonicalEventSink`, the controller serializes and
validates immutable bytes before queue handoff; later mutation of a third-party
event cannot change the evidence. Sink I/O remains isolated from ordinary
device ingest by default. `WithSynchronousAudit` instead waits for every sink
callback before committing Snapshot state, exposing the event, or running
processors. If admission or callback completion fails, the live event is not
exposed and the controller terminates into its neutral terminal sequence.

Queue acceptance is not durability. `Controller.RecordCommandSync` is the
stronger barrier: it waits for every attached sink callback and, because each
sink is FIFO, every earlier admitted event. `audit.Recorder` flushes and, when
the writer supports `Sync`, synchronizes its store during that callback by
default. Cancellation after controller-queue admission is explicitly
indeterminate through
`ErrCommandPublicationUncertain`; cancellation cannot retract already admitted
work. `assured.Session` forces synchronous audit for every event, verifies the
recorder's durable high-water mark, and only then exposes state or permits
requested actuation.

If a sink fails, times out, or exhausts its queue, the controller terminates,
publishes canonical neutral/release events to subscribers, and exposes the
error through `Done`, `Err`, and `Close`. Terminal subscriber delivery remains
best effort even when the failed evidence sink cannot retain those final
events; the Assured Session reports finalization failure instead of claiming a
clean session.

A lossless subscription never silently drops an event: only that subscription
terminates with `teleop.ErrSubscriptionOverflow` if its configured queue is
exhausted. Latest-value subscriptions account for coalescing in
`Subscription.Stats`; subscription loss is also sent to authoritative sinks as
a `GapEvent`.

This guarantee begins at the OS API boundary. Linux evdev and macOS Game
Controller provide event-oriented streams. Windows XInput exposes sampled
state, so its descriptors advertise `teleop.AuditSampledState`.

These backends are change-driven: a control held steadily may produce no new
observation. `SnapshotWithMeta` separates state-change time, observation age,
and independently sampled Transport Health. A `TransportHealthSource` advances
a sequence after each fresh check at its documented OS/framework seam; silence
is explicitly unverifiable when that capability is absent. Linux uses
`EVIOCGID` to check that the kernel still recognizes the retained evdev
attachment. macOS checks exact retained-controller membership in
`GCController.controllers`. These checks expose OS/framework-reported
connection and disconnect detection; they are not physical-device or radio
challenge/response. Confirmed backend disconnect always neutralizes state;
age-based neutralization is an explicit application option.
Headers include a session-relative monotonic reading, and the controller emits
`ClockEvent` if wall time steps materially relative to that reading. Application
commands receive controller-owned IDs, preserving their causal place in the
same stream.

## Assured actuation guarantee

`safety.Authority` serializes lifecycle and Apply operations so an Emergency
Stop cannot race a stale Evaluate-to-actuator window. Each attempt records a
decision and intent before sending a controller-session-bound, increasing
sequence with a finite Command Lease. The receiver must enforce expiry and
ordering. Send failure, rejection, timeout, missing/inconsistent application
acknowledgement, evidence failure, or authority change selects a newer
Engineered Safe State command.

`assured.Session` is the production-oriented composition. It requires an exact
backend stream, independently verifiable silence at the backend's documented
OS/framework connection seam, domain-neutral `safety.StrictConfig`, sync-capable
exclusive evidence storage, Ed25519 signing, external witnessing, required
application and caller-declared operator/authorization provenance, the
effective teleop policy, a live-command `Authority.Policy`, applied acknowledgements, and a receiver-enforced
actuator lease. Every admitted event crosses the local sync
barrier before live exposure, startup requires the exact newly created
checkpoint to be witnessed, and orderly close requires a witnessed signed
footer. The raw controller, Guard, Recorder, and actuator are not exposed by
the Session.

The guarantee stops at the adapter seams. Hardware emergency stop, physical
feedback, signer/witness custody, storage retention, clock trust, actuator
lease enforcement, authenticated operator authorization, system-command
mapping and limits, adapter identity/configuration retention, and
system-specific safe-state analysis are deployment responsibilities. Authority
preserves exact command bytes and input causality and delegates semantic checks
to the required policy adapter. Its reference implementation has strict scalar
limits and independently signed grants, not built-in vehicle limits. A custom
policy's permission, store's `Sync` result and Anchor's successful return are
adapter claims, not independently verifiable properties of this process.

Assured `Config.Processors` supports ordered gesture/action composition after
the authority processor; exact instances and order are attested. Startup
contexts do not own an established session: `Session.Close` owns evidenced
shutdown. `simulation.Receiver` is an explicit deterministic reference model;
manual-clock construction never weakens Assured's system-clock requirement.

## Extending teleop

A third-party provider implements `teleop.Provider` and returns a
`teleop.InputSource` wrapped with `teleop.NewController`. A source that
advertises rumble also implements `teleop.RumbleSource`; the controller exposes
it through `GameController.SetRumble`. New control IDs and controller types can
be introduced as string-backed values without registering them globally.

Provider implementations should:

1. Preserve every OS reading.
2. Normalize sticks to `[-1,+1]`, with positive Y meaning up.
3. Normalize triggers to `[0,1]`.
4. Report actual capabilities instead of synthesizing missing controls.
5. Turn known loss into `Observation.Gap`.
6. Make `Close` idempotent and unblock `Read`.
7. If rumble is advertised, accept concurrent `SetRumble` calls and stop both
   motors before `Close` releases the device.
8. Implement `TransportHealthSource` only when the adapter can independently
   and freshly check a documented OS/framework connection condition while
   state is unchanged. Advance its sequence for each completed check, timestamp
   it, report confirmed disconnection at that seam, and disclose detection
   latency and what lies beyond the seam. Do not manufacture health from an
   application timer or call an OS attachment check a physical-link response.

Providers that can observe hotplug directly or by inexpensive polling may also
implement `teleop.WatchingProvider`. Watching is explicit and owns no global
goroutine.
