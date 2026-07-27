# Architecture

Teleop separates controller-independent application code from controller- and
operating-system-specific input.

```text
OS input API
    ↓
xbox.InputSource
    ↓
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

- `teleop` owns controller-neutral controls, states, events, subscriptions,
  provider interfaces, normalization, and lifecycle semantics.
- `xbox` supplies Xbox labels and selects the platform backend.
- `gesture` derives temporal patterns without hiding canonical input.
- `action` maps physical or gesture events to application-defined identifiers.
- `audit` stores the event and causality chain.
- `testkit` supplies deterministic fake and replay sources.

Attach recognizers and mappers with `teleop.WithProcessor`. Processors run in
option order, and their output is published to the same subscribers and audit
sinks as canonical input.

The core module does not pair devices. Bluetooth, USB, and Xbox Wireless
connections are managed by the host operating system.

## Event guarantee

Every observation delivered by an `InputSource` is recorded to authoritative
event sinks before it is published to subscriptions. A lossless subscription
never silently drops an event: it terminates with
`teleop.ErrSubscriptionOverflow` if its configured queue is exhausted.

This guarantee begins at the OS API boundary. Linux evdev and macOS Game
Controller provide event-oriented streams. Windows XInput exposes sampled
state, so its descriptors advertise `teleop.AuditSampledState`.

## Extending teleop

A third-party provider implements `teleop.Provider` and returns an
`teleop.InputSource` wrapped with `teleop.NewController`. New control IDs and
controller types can be introduced as string-backed values without registering
them globally.

Provider implementations should:

1. Preserve every OS reading.
2. Normalize sticks to `[-1,+1]`, with positive Y meaning up.
3. Normalize triggers to `[0,1]`.
4. Report actual capabilities instead of synthesizing missing controls.
5. Turn known loss into `Observation.Gap`.
6. Make `Close` idempotent and unblock `Read`.

Providers that can observe hotplug directly or by inexpensive polling may also
implement `teleop.WatchingProvider`. Watching is explicit and owns no global
goroutine.
