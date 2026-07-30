# Architecture

Teleop separates controller-independent application code from controller- and
operating-system-specific input and feedback output.

```text
OS controller API
    ↕
xbox device source
    ↕
teleop.Controller
    ├── authoritative EventSink (audit)
    ├── lossless subscription
    └── latest-value subscription
          ↓
      gesture.Recognizer
          ↓
       action.Mapper
```

## Package boundaries

- `teleop` owns controller-neutral controls, states, rumble output, events,
  subscriptions, provider interfaces, normalization, and lifecycle semantics.
- `xbox` supplies Xbox labels and selects the platform backend.
- `gesture` derives temporal patterns without hiding canonical input.
- `action` maps physical or gesture events to application-defined identifiers.
- `audit` stores the event, command, and causality chain.
- `testkit` supplies deterministic fake and replay sources.

Attach recognizers and mappers with `teleop.WithProcessor`. Processors run in
option order, and their output is published to the same subscribers and audit
sinks as canonical input.

The core module does not pair devices. Bluetooth, USB, and Xbox Wireless
connections are managed by the host operating system.

## Event guarantee

Every observation delivered by an `InputSource` is accepted into each bounded
authoritative-sink queue before it is published to subscriptions. Sink I/O is
isolated from device ingest, so a durable recorder cannot add filesystem or
`fsync` latency to the control path. If a sink fails, times out, or exhausts
its queue, the controller terminates, publishes canonical neutral/release
events to subscribers, and exposes the error through `Done`, `Err`, and
`Close`. Acceptance does not mean the record is already durable when a
subscriber receives the event.

A lossless subscription never silently drops an event: only that subscription
terminates with `teleop.ErrSubscriptionOverflow` if its configured queue is
exhausted. Latest-value subscriptions account for coalescing in
`Subscription.Stats`; subscription loss is also sent to authoritative sinks as
a `GapEvent`.

This guarantee begins at the OS API boundary. Linux evdev and macOS Game
Controller provide event-oriented streams. Windows XInput exposes sampled
state, so its descriptors advertise `teleop.AuditSampledState`.

These backends are change-driven: a control held steadily may produce no new
observation. `SnapshotWithMeta` therefore exposes observation age without
claiming that age proves transport failure. Confirmed disconnect always
neutralizes state; age-based neutralization is an explicit application option.
Headers include a session-relative monotonic reading, and the controller emits
`ClockEvent` if wall time steps materially relative to that reading. Application
commands are injected through `GameController.RecordCommand` and receive a
controller-owned event ID, preserving their causal place in the same stream.

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

Providers that can observe hotplug directly or by inexpensive polling may also
implement `teleop.WatchingProvider`. Watching is explicit and owns no global
goroutine.
