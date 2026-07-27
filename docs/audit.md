# Auditing controller input

Attach a recorder when opening a controller:

```go
file, err := os.OpenFile("controller.jsonl", os.O_CREATE|os.O_WRONLY, 0o600)
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

The audit log is versioned newline-delimited JSON. Each record contains its
event kind, full event payload, controller session and sequence, observation
time, causes, and a SHA-256 hash linked to the preceding record.

Use `audit.ReadAll` to decode and verify a completed stream. Modification,
reordering, deletion, or truncation causes verification to fail. Use
`audit.ReadPartial` only when inspecting an interrupted or currently open log.

`audit.Descriptor` and `audit.Observations` extract the controller metadata and
canonical observations. Pass those observations to `testkit.NewReplaySource`
to reproduce the input stream without controller hardware.

## Guarantee boundary

The recorder stores every event received from the selected backend. It cannot
store input that the operating system or transport did not expose.

- `AuditExactBackendStream` means the backend consumes an OS event stream.
- `AuditSampledState` means intermediate transitions can occur between polls.
- `GapEvent` means the backend or a queue reported known input loss.

An application that requires an uninterrupted trail should treat a `GapEvent`,
subscription overflow, recorder error, or sampled backend as a safety event.
Teleop reports these conditions; the application decides the appropriate safe
state.
