# teleop

[![Go Reference](https://pkg.go.dev/badge/github.com/open-ships/teleop.svg)](https://pkg.go.dev/github.com/open-ships/teleop)

Normalized, loss-aware game-controller input for Go teleoperation and autonomy
applications.

`teleop` separates controller hardware and operating-system APIs from
application control logic. It exposes transport-independent state snapshots,
an ordered event stream, optional gestures and semantic actions, fail-closed
output gating, and signed, hash-chained audit recording. The included `xbox`
provider works on Linux, macOS, and Windows.

> [!IMPORTANT]
> `teleop` transports operator input and can gate it, but it is not a certified
> safety controller. The `safety` package provides a command timeout, dead-man
> switch, loop watchdog, and latching emergency stop. It does not replace a
> hardware emergency stop, a safety-rated interlock, or an actuator that reaches
> a safe state on its own when commands stop arriving — that last one is the
> control that actually protects people, and it lives in the receiving system,
> not here. A backend cannot report input that the operating system, driver,
> radio, or polling API never delivered.

The API has completed its pre-v1 review, and hardware mappings have been
validated across the supported controller generations and operating systems.
The `v1.0.0` tag begins the Go module compatibility commitment.

## Features

- Controller-neutral `GameController`, `Provider`, `State`, and `Event` APIs
- Normalized sticks, triggers, buttons, and four-way D-pad with diagonals
- Xbox A/B/X/Y aliases without coupling application code to printed labels
- Typed input, connection, capability, error, and known-loss events
- Multiple controllers, fan-out subscriptions, and explicit hotplug watching
- Lossless delivery for command consumers and latest-value delivery for UIs
- Tap, double-tap, hold, chord, stick-region, and trigger-threshold gestures
- Application-defined action bindings with causal event IDs
- Application command records linked to the input that caused them
- Command timeout, dead-man switch, and loop watchdog that fail closed
- Monotonic event timing and wall-clock step detection
- Append-only, hash-chained JSON Lines audit logs
- Ed25519-signed Merkle tree heads, checkpoints, and external anchoring
- Build, configuration, and operator provenance recorded per session
- Fake and replay input sources for tests
- Interactive terminal monitor and machine-readable JSON output

The library packages do not install a global registry. Platform syscall
wrappers use `golang.org/x/sys`; the optional terminal monitor uses Bubble Tea
and Lip Gloss.

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

`SnapshotWithMeta` also returns the last physical observation and receive
times, connection state, and whether the snapshot was synthesized. Use those
timestamps to enforce an application-specific command timeout. Game-controller
backends are change-driven, so a held control can legitimately produce no new
observations; observation age alone is not proof of disconnect. Confirmed
disconnects always synthesize a neutral state and release events.

## Input model

Controls are named by physical position so application bindings remain stable
across controller brands. For example, `xbox.ButtonA` is an alias for
`teleop.ButtonFaceSouth`.

- Stick axes are in `[-1, +1]`; positive X is right and positive Y is up.
- Triggers are in `[0, 1]`.
- D-pad directions are independent booleans, so diagonals are preserved.
- D-pad direction changes emit `ButtonEvent` values such as
  `button.dpad.left` with `pressed` or `released` phases.
- `Descriptor.Capability` reports only the controls exposed by the selected
  backend.
- Canonical input is not coalesced and has no dead zone applied. Apply
  `teleop.ApplyRadialDeadZone` only when turning input into commands.

Each backend observation produces an `ObservationEvent`, followed by any
corresponding `ButtonEvent`, `StickEvent`, or `TriggerEvent`.
Events carry a controller session, sequence number, observation time, and
causal event IDs. They also carry a session-relative monotonic timestamp, so
ordering and durations remain meaningful if the host wall clock is corrected;
a material correction is reported as a `ClockEvent`.

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

Record the application command at the same decision boundary that sends it to
the machine. Link it to the input or derived event IDs that produced it so the
audit trail records both operator intent and the command actually considered:

```go
err := controller.RecordCommand(ctx, teleop.Command{
    Name:       "drive.set",
    Payload:    map[string]float64{"forward": forward},
    Authorized: armed,
    Causes:     []teleop.EventID{event.Header().ID},
})
```

`RecordCommand` is non-blocking; `teleop.ErrPipelineOverflow` means the
command could not be admitted to the bounded audit pipeline and should be
treated as a safety fault.

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

The controller accepts every canonical and derived event into each bounded
audit queue before delivering it to subscribers. Sink I/O runs independently
so filesystem latency does not delay the input path:

```go
file, err := os.OpenFile(
    "controller.jsonl",
    os.O_CREATE|os.O_EXCL|os.O_WRONLY,
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

`audit.ReadAll` verifies a completed log's integrity chain and footer. An
unkeyed SHA-256 chain detects corruption but can be recomputed by an attacker;
use `audit.WithHMAC(key)` and `audit.ReadAuthenticated` when adversarial
tamper-evidence is required. Use `audit.ReadPartial` for a running or
interrupted log. `audit.Observations` and `testkit.NewReplaySource` can then
reproduce its canonical input without controller hardware.
For forensic playback, `audit.Events` decodes the recorded canonical,
gesture, action, command, clock, and lifecycle events directly, preserving
their original identity and timing instead of re-deriving them.

Where a log may become evidence, integrity is not enough. Sign it, anchor it,
and record what interpreted the input:

```go
recorder := audit.NewRecorder(
    file,
    audit.WithSigner(signer),        // Ed25519; prefer a TPM or HSM key
    audit.WithCheckpoints(10*time.Second, 10000),
    audit.WithAnchor(anchor),        // publish heads outside this host
    audit.WithProvenance(provenance),
)
```

Each mechanism closes a different gap. The hash chain shows the log was not
edited; `WithHMAC` shows a key holder wrote it; `WithSigner` lets an
independently trusted public key authenticate signed tree heads without giving
the verifier signing capability; `WithAnchor` makes a destroyed or truncated
log detectable rather than merely suspected; and `WithProvenance` records the
build, configuration, and operator without which recorded input cannot be
turned back into behavior.
Records also form an RFC 6962 Merkle tree, so a single record can be proved to
a signed head without disclosing the rest of the log.

Verify completed signed logs with
`audit.ReadTrusted(reader, trustedPublicKey)`. The public key must come from a
separate trusted channel; a key declared only inside the log proves internal
consistency, not device identity. Key provisioning, rotation, revocation, and
destruction are deployment responsibilities.

When feeding a finite `testkit.ReplaySource` through a controller, pass
`teleop.WithDeferredStart()` so `Subscribe` is attached before replay begins.

Known backend loss is published as `GapEvent`. Recorder failures stop the
controller pipeline, and lossless subscriber overflow is explicit. See
[the audit guide](docs/audit.md) for the full guarantee boundary and replay
workflow.

Every mechanism above detects records that were altered, removed, or
misattributed. None detects input the operating system never delivered, which
is the failure most likely to injure someone. That is what the next section is
for.

## Command timeout and dead-man switch

The `safety` package gates output on conditions it can affirmatively establish,
and inhibits on anything it cannot:

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
)
if err != nil {
    return err
}
guard.Bind(controller)

for range ticker.C {
    guard.Heartbeat()
    decision := guard.Evaluate()
    drive(decision.Command) // neutral whenever inhibited
}
```

`Evaluate` is authoritative and must be called immediately before acting.
`Decision.Command` is the neutral state whenever output is inhibited, so a
stale decision cannot leak live input into an actuator. Input age is measured
on a monotonic clock, so a wall-clock step cannot make stale input look fresh.

Faults latch: a dropout revokes authority until an operator calls `Arm` again,
because automatic resumption is how a transient dropout becomes an unexpected
movement. Releasing the dead-man control is normal operation and does not
latch, while holding it past the re-actuation deadline is treated as a defeated
switch and does. The guard also re-evaluates on the controller's own ticker, so
it trips even if the application stops calling `Evaluate`.

Transitions are published into the same subscriptions and audit sinks as input.
Pair the guard with `controller.RecordCommand` so the log shows both what the
operator did and what the gate permitted.

A `safety.Guard` is not a certified safety controller and does not replace a
hardware emergency stop or an actuator that reaches a safe state on its own
when commands stop arriving. See [the safety guide](docs/safety.md).

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

The interactive TUI uses latest-value delivery and reports any coalesced
events, keeping the recent canonical event list responsive. Use `--device ID`
to select a controller. To losslessly stream every published event, including
raw observations, as JSON Lines:

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
| `safety` | Command timeout, dead-man switch, and fail-closed output gating |
| `audit` | Signed, hash-chained JSON Lines recording, verification, and replay extraction |
| `testkit` | Deterministic fake and replay input sources |

Further reading:

- [Architecture guide](docs/architecture.md) — implementing another provider
- [Safety guide](docs/safety.md) — command timeout, dead-man switch, and what
  they do not cover
- [Audit guide](docs/audit.md) — what each integrity mechanism actually proves,
  and the guarantee boundary

## License

MIT
