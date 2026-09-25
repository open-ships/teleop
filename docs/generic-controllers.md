# Generic controllers and SNES layouts

`generic.Provider` implements the same `teleop.Provider` and optional hotplug
interfaces as `xbox.Provider`. Its `Open` method returns the same
`teleop.GameController`, accepting the same controller options, processors,
subscriptions and audit sinks. Xbox input backends remain available separately.

The first layout is `profiles.SNES`: four face buttons, L/R, Start/Select and
four D-pad directions. A generic **device mapping** translates a backend's raw
buttons, axes and hats into these physical positions. A **profile** supplies
positions and printed labels. Selecting the SNES profile does not guess a USB
controller's button numbering.

## Use the monitor

Connect your controllers through the operating system, then run:

```sh
go run ./cmd/teleop-monitor
```

Every interactive launch lists connected controllers and asks which one to use.
Enter its number; `q` exits. Xbox controllers open directly. For an unmapped
generic controller, the monitor first guides you through setup and then opens
the live monitor without another command.

Setup asks you to release all controls and press Enter, then to hold and release
each of twelve controls in turn. Press only the requested button or cardinal
D-pad direction. Hold it until setup reports that it was captured. Ctrl-C
cancels; each press/release step has a 45-second timeout. Duplicate assignments
are rejected.

Completed mappings are saved separately by device identity, raw input layout
and profile under `teleop/mappings` in the OS user configuration directory
(`os.UserConfigDir`). They are loaded automatically on future runs. Connecting
another controller does not replace existing devices' mappings, and the monitor
does not remember your controller selection. A cancelled or incomplete setup
does not save a mapping. If saving fails, the learned mapping can still be used
for the current session and the monitor reports that it could not save it.

Generic mappings currently use the SNES layout. No raw button numbering is
guessed. An unmapped device appears as “needs mapping” in the picker. The input
panel uses profile labels and omits absent sticks, triggers and buttons.
Generic rumble is currently unsupported.

`--device ID` skips the picker. `--provider xbox` or `--provider generic` filters
it. `--list` prints devices and mapping readiness without starting setup.
`--json` and redirected output never prompt; they automatically use saved
mappings, or report `generic.ErrMappingRequired` when a device needs interactive
setup. This keeps JSON output machine-readable.

## Export or supply a mapping explicitly

For application configuration or a manual override, the existing commands are
still available:

```sh
go run ./cmd/teleop-monitor --configure snes.json
go run ./cmd/teleop-monitor --mapping snes.json
```

`--configure` is a standalone export command. If several generic pads are
connected, add `--device ID`. It writes a mapping only after every control is
validated, and never overwrites an existing file. `--mapping` takes precedence
over the automatically loaded mapping for the same device identity and profile.

The generic library itself does not read or write global configuration.
Applications pass mappings explicitly. Unknown devices remain discoverable;
opening one without a compatible mapping returns `generic.ErrMappingRequired`.

## Integrate in Go

```go
file, err := os.Open("snes.json")
if err != nil {
    return err
}
mapping, err := generic.LoadMapping(file)
closeErr := file.Close()
if err != nil {
    return err
}
if closeErr != nil {
    return closeErr
}

provider := generic.NewProvider(
    generic.WithProfile(profiles.SNES),
    generic.WithMapping(mapping),
)
devices, err := provider.Discover(ctx)
// Handle errors, select a descriptor with mapping_status=ready.
controller, err := provider.Open(ctx, selected.ID,
    teleop.WithProcessor(actions),
    teleop.WithAuditSink(recorder),
)
// Subscribe, Snapshot, SetRumble and Close have their existing signatures.
// SetRumble returns ErrUnsupported when capabilities do not advertise it.
```

See the complete runnable [example](../examples/generic/main.go). Multiple
mappings can be registered with repeated `WithMapping` options. Multiple
matches for a device/profile produce an error rather than choosing arbitrarily.
Options and discovered descriptors are copied to prevent caller mutation from
changing active mappings.

For mixed providers, use the existing registry:

```go
registry := teleop.NewRegistry(xbox.NewProvider(), provider)
devices, err := registry.Discover(ctx)
controller, err := registry.Open(ctx, selected.Type, selected.ID, options...)
```

A mapping is scoped to backend, vendor/product IDs where available, device name,
and a SHA-256 fingerprint of raw input IDs, kinds and logical ranges. Attachment
paths are excluded so reconnecting the same layout does not require setup again.
Different backend representations require their own mapping. Controllers with
identical identities and descriptors but different internal wiring still need
separate configuration; descriptors cannot reveal printed button labels.
Profile ID, input-layout fingerprint and a digest of the applied mapping are
included in the controller descriptor and therefore its recorded capabilities.

## Button positions and application actions

| Position | SNES label / alias | Xbox label |
|---|---|---|
| South | B / `snes.ButtonB` | A |
| East | A / `snes.ButtonA` | B |
| West | Y / `snes.ButtonY` | X |
| North | X / `snes.ButtonX` | Y |
| Left bumper | L / `snes.LeftShoulder` | LB |
| Right bumper | R / `snes.RightShoulder` | RB |
| Primary menu | Start / `snes.Start` | Menu |
| Secondary menu | Select / `snes.Select` | View |

Bind `teleop.ButtonFaceSouth` when physical position is the intended meaning.
Use `snes.ButtonA` when the SNES printed label is the intended meaning. An Xbox
alias still names its original physical position and is not relabeled by a
profile.

A D-pad is kept digital, including diagonals. It does not impersonate an analog
stick. Applications using sticks or triggers must provide alternate D-pad or
button action bindings. The current mapping engine supports digital layouts;
custom profiles may contain buttons and D-pad directions, including extension
buttons. Analog stick/trigger mapping is a future extension.

## Backends and delivery guarantees

| Platform | Backend | Requirements and limits |
|---|---|---|
| macOS | IOKit HID | cgo; joystick/gamepad collections; no extra runtime library |
| Linux | evdev state queries | permission to read `/dev/input`; at least four joystick/gamepad buttons |
| Windows | WinMM joystick compatibility API | OS joystick driver; up to 32 buttons, six axes and one POV |

These generic backends sample state approximately every 8 ms. They advertise
`AuditSampledState`: short presses or intermediate transitions between samples
can be missed, and there is no exact event-stream claim. Observations are emitted
when raw state changes. Saving every emitted event cannot recover transitions
that sampling did not observe. Linux's existing Xbox evdev event backend keeps
its existing event-stream behavior.

While controls remain held, each generic sample independently checks the
backend's documented OS attachment seam. macOS checks the retained I/O Registry
entry; Linux issues `EVIOCGID` on the retained descriptor; Windows checks joystick
capabilities and polls the slot. This reports OS attachment, not physical-link
challenge/response. WinMM slot identity can be reused; its driver manufacturer
and product identifiers are stored as properties, not falsely presented as USB
vendor/product IDs. Unplug/error handling goes through the existing controller
lifecycle, including synthetic releases on disconnect. Existing strict/assured
requirements still apply to the selected backend and application configuration.

Generic discovery reserves Microsoft USB vendor IDs and Xbox/Microsoft-named
devices for the Xbox provider. On APIs that conceal a device's identity, name
matching is best effort; explicit provider selection is available. On macOS
without cgo, or other unsupported platforms, generic discovery/opening returns
`teleop.ErrUnsupported`.

Native references: [Apple IOHIDDeviceGetValue](https://developer.apple.com/documentation/iokit/1588657-iohiddevicegetvalue),
[Windows joyGetPosEx](https://learn.microsoft.com/en-us/windows/win32/api/joystickapi/nf-joystickapi-joygetposex).

## Build a configuration tool

`Provider.Inspect` returns each device's raw control descriptors.
`Provider.OpenRaw` provides complete sampled states without assuming a layout.
`generic.DetectBinding` detects a single press relative to a released baseline,
including axis-based D-pads and four/eight-position hats. Save learned bindings
with `NewMapping`, `Mapping.Validate` and `SaveMapping`; load with `LoadMapping`.

Both sampled input and configuration tools must retain the raw logical ranges.
For axis-based digital controls, a press engages halfway from center to the
endpoint. Hat directions preserve diagonal combinations during normal use;
setup learns only cardinal directions. An incomplete, ambiguous, wrong-device,
or incompatible mapping cannot open a normalized controller.
