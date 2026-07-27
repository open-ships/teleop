# teleop

Normalized, loss-aware game-controller input for Go teleoperation and autonomy
applications.

`teleop` separates controller hardware and operating-system APIs from
application control logic. It exposes transport-independent state snapshots,
an ordered event stream, optional gestures and semantic actions, and
hash-chained audit recording. The included `xbox` provider works on Linux,
macOS, and Windows.

> [!IMPORTANT]
> `teleop` transports operator input; it is not a safety controller. Applications
> must provide their own authorization, command timeout, dead-man switch,
> emergency-stop, and safe-state behavior. A backend cannot report input that
> the operating system, driver, radio, or polling API never delivered.

The API is pre-v1 while hardware mappings are validated across controller
generations and operating systems.

## Features

- Controller-neutral `GameController`, `Provider`, `State`, and `Event` APIs
- Normalized sticks, triggers, buttons, and four-way D-pad with diagonals
- Xbox A/B/X/Y aliases without coupling application code to printed labels
- Typed input, connection, capability, error, and known-loss events
- Multiple controllers, fan-out subscriptions, and explicit hotplug watching
- Lossless delivery for command consumers and latest-value delivery for UIs
- Tap, double-tap, hold, chord, stick-region, and trigger-threshold gestures
- Application-defined action bindings with causal event IDs
- Append-only, hash-chained JSON Lines audit logs
- Fake and replay input sources for tests
- Interactive terminal monitor and machine-readable JSON output

The library packages use only the Go standard library and do not install a
global registry. The optional terminal monitor uses Bubble Tea and Lip Gloss.

## Install

`teleop` currently requires Go 1.25 or newer.

```sh
go get github.com/open-ships/teleop
```

Pair or connect controllers through the host operating system first. The
package does not implement Bluetooth or Xbox Wireless pairing.

## Quick start

Discover a controller, subscribe to its event stream, and consume normalized
input:

```go
provider := xbox.NewProvider()
devices, err := provider.Discover(ctx)
if err != nil {
    return err
}
if len(devices) == 0 {
    return errors.New("no Xbox controller connected")
}

controller, err := provider.Open(ctx, devices[0].ID)
if err != nil {
    return err
}
defer controller.Close()

events, err := controller.Subscribe(teleop.SubscriptionOptions{
    Delivery: teleop.DeliveryLossless,
    Buffer:   1024,
})
if err != nil {
    return err
}
defer events.Close()

for {
    event, err := events.Next(ctx)
    if err != nil {
        return err
    }

    switch event := event.(type) {
    case teleop.ButtonEvent:
        log.Printf("%s %s", event.Button, event.Phase)
    case teleop.StickEvent:
        log.Printf(
            "%s stick: x=%+.3f y=%+.3f",
            event.Stick,
            event.Position.X,
            event.Position.Y,
        )
    case teleop.TriggerEvent:
        log.Printf("%s trigger: %.3f", event.Trigger, event.Position)
    }
}
```

See the complete runnable example in
[`examples/basic`](examples/basic/main.go).

For applications that need current intent rather than every transition, take a
thread-safe snapshot:

```go
state := controller.Snapshot()
forward := teleop.ApplyRadialDeadZone(state.LeftStick, 0.12).Y
armed := state.Button(xbox.ButtonA)
```

## Input model

Controls are named by physical position so application bindings remain stable
across controller brands. For example, `xbox.ButtonA` is an alias for
`teleop.ButtonFaceSouth`.

- Stick axes are in `[-1, +1]`; positive X is right and positive Y is up.
- Triggers are in `[0, 1]`.
- D-pad directions are independent booleans, so diagonals are preserved.
- `Descriptor.Capability` reports only the controls exposed by the selected
  backend.
- Canonical input is not coalesced and has no dead zone applied. Apply
  `teleop.ApplyRadialDeadZone` only when turning input into commands.

Each backend observation produces an `ObservationEvent`, followed by any
corresponding `ButtonEvent`, `StickEvent`, `TriggerEvent`, or `DPadEvent`.
Events carry a controller session, sequence number, observation time, and
causal event IDs.

### Delivery policies

Each call to `Subscribe` creates an independent bounded queue:

- `DeliveryLossless` terminates with `teleop.ErrSubscriptionOverflow` instead
  of silently dropping input. Use it for command and safety consumers.
- `DeliveryLatest` discards the oldest queued event when full. Use it for
  monitors and other snapshot-oriented consumers.

The default buffer is 1024 events. Choose a buffer based on the consumer's
worst-case latency and always handle the subscription's terminal error.

## Gestures and actions

Processors derive higher-level events without hiding the canonical input.
Attach the gesture recognizer before the action mapper because processors run
in option order:

```go
recognizer := gesture.New(gesture.DefaultConfig())
mapper := action.New(
    action.OnGesture("arm", gesture.Hold, xbox.ButtonA),
    action.OnButton("disarm", xbox.ButtonB, teleop.PhasePressed),
    action.OnStick("drive", teleop.LeftStick),
)

controller, err := provider.Open(
    ctx,
    devices[0].ID,
    teleop.WithProcessor(recognizer),
    teleop.WithProcessor(mapper),
)
```

Recognized `gesture.Event` and mapped `action.Event` values appear in the same
subscriptions and audit sinks as the input events that caused them.

## Discovery and hotplug

`Discover` returns a descriptor for each connected controller. The Xbox
provider also implements `teleop.WatchingProvider`:

```go
for event := range provider.Watch(ctx) {
    switch event.Kind {
    case teleop.DeviceAdded:
        log.Printf("connected: %s", event.Descriptor.ID)
    case teleop.DeviceRemoved:
        log.Printf("disconnected: %s", event.Descriptor.ID)
    case teleop.DeviceError:
        log.Printf("controller discovery: %s", event.Error)
    }
}
```

Watching is opt-in and stops when `ctx` is canceled. Multiple simultaneous
controllers are exposed independently when the OS provides a distinct device
or XInput slot.

## Platform support

| Platform | Backend | Audit grade | Notes |
| --- | --- | --- | --- |
| Linux | evdev | `AuditExactBackendStream` | Direct `/dev/input` event stream; no `libudev` dependency |
| macOS | Game Controller | `AuditExactBackendStream` | Requires cgo and Apple's Foundation and GameController frameworks |
| Windows | XInput | `AuditSampledState` | Polls up to four XInput slots; physical transport is not exposed |

Capabilities vary by controller, driver, and OS. Guide/Xbox, Share, and Elite
paddles are reported only when the backend exposes them. Standard XInput does
not expose these controls.

### Linux setup

The application needs read permission for `/dev/input/event*`.

- USB controllers normally use the kernel `xpad` driver.
- Bluetooth controllers commonly use
  [`xpadneo`](https://github.com/atar-axis/xpadneo).
- The Xbox Wireless Adapter requires a compatible GIP driver such as
  [`xone`](https://github.com/medusalix/xone) or a maintained successor.

The Linux backend detects evdev `SYN_DROPPED`, resynchronizes its state, and
emits a `GapEvent`.

## Auditing and replay

An audit sink receives every canonical and derived event before subscribers:

```go
file, err := os.OpenFile(
    "controller.jsonl",
    os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
    0o600,
)
if err != nil {
    return err
}
defer file.Close()

recorder := audit.NewRecorder(file, audit.WithFlushEveryEvent(true))
defer recorder.Close()

controller, err := provider.Open(
    ctx,
    deviceID,
    teleop.WithAuditSink(recorder),
)
```

`audit.ReadAll` verifies a completed log's hash chain and footer. Use
`audit.ReadPartial` for a running or interrupted log. `audit.Observations` and
`testkit.NewReplaySource` can then reproduce its canonical input without
controller hardware.

Known backend loss is published as `GapEvent`. Recorder failures stop the
controller pipeline, and lossless subscriber overflow is explicit. See
[the audit guide](docs/audit.md) for the full guarantee boundary and replay
workflow.

## Terminal monitor

List controllers:

```sh
go run ./cmd/teleop-monitor --list
```

Open the first controller in the interactive monitor:

```sh
go run ./cmd/teleop-monitor
```

The Bubble Tea monitor uses the alternate screen, switches between
side-by-side and stacked layouts as the terminal is resized, and exits
immediately with `q`, `Esc`, or `Ctrl-C`.

Record while monitoring:

```sh
go run ./cmd/teleop-monitor --audit controller.jsonl
```

The TUI losslessly consumes and counts every event while keeping the recent
canonical event list readable instead of filling it with raw observations. Use
`--device ID` to select a controller. To stream every published event,
including raw observations, as JSON Lines:

```sh
go run ./cmd/teleop-monitor --json
```

Output automatically switches to the same JSON Lines stream when stdout is
redirected.

## Packages

| Package | Purpose |
| --- | --- |
| `teleop` | Controller-neutral API, event runtime, normalization, and registry |
| `xbox` | Cross-platform Xbox provider and familiar control aliases |
| `gesture` | Optional gesture recognition |
| `action` | Optional mapping from physical input or gestures to semantic actions |
| `audit` | Hash-chained JSON Lines recording, verification, and replay extraction |
| `testkit` | Deterministic fake and replay input sources |

See [the architecture guide](docs/architecture.md) to implement another
controller provider.

## License

MIT
