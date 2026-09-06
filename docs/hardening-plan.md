# Mission-critical hardening work and acceptance evidence

The objective is to resolve the review findings and establish testable software
guarantees for high-risk use. The package's scope is controller input and
auditing with optional domain-neutral strict actuation; completing library work
does not require receiver specifications. Approving a physical deployment
separately requires installation-specific evidence: no generic command envelope
or engineered safe state can substitute for that evidence. See
[integration choices](integration.md) and ADR 0004.

## Software work

| Requirement | Acceptance evidence | Status |
| --- | --- | --- |
| Reject invalid raw Linux input before normalization; validate axis metadata | `xbox/input_validation_linux_test.go`; existing controller invalid-state admission tests; valid endpoints preserved | Software tests passed |
| Never publish a partially resynchronized Linux state | Required-axis query failures/invalid values leave state and calibration unchanged | Software tests passed |
| Superseded unsent intent does not accidentally disarm; actual uncertainty still inhibits | `safety/supersession_test.go`, `safety/policy_test.go`, `assured/policy_integration_test.go`; terminal unsent evidence and bounded unfinished-chain bookkeeping | Software tests passed |
| Preserve configuration exactly when cloned and verified | `audit/mission_critical_test.go`: overlapping slices and large-number signed-log round trips | Software tests passed |
| Forensic replay handles legacy errors and unknown extensions without panic or data loss | Legacy placeholder and opaque-event round-trip tests | Software tests passed |
| Centralize vessel command policy and record its decisions | Strict identity, signed grants, canonical numbers, units, ranges, transitions, setpoint deltas, malformed configuration and Assured-to-receiver integration | Reference policy passed; actual limits/authorization service open |
| Provide receiver enforcement and simulation at the actuator seam | `simulation/receiver_test.go`, executable example and Assured integration; expiry, boot epoch, ownership, order/replay, grant/lease violations, fallback and simulated receipts | Model passed; real receiver/feedback open |
| Support safe composition and deterministic simulation without weakening production assurance | Explicit simulation construction, processor composition, provider startup-context ownership, pipeline attestation tests | Software tests passed |
| Provide incident verification and independent evidence retention seams | `audit/witness_verify_test.go`, mirror and CLI tests; signed forks/truncation, duplicate witness fields, withheld failed timelines | Software tests passed; actual custody/storage acceptance open |
| Improve continuous verification and patched tooling | Native/no-cgo tests, Linux race execution, Windows cross-build, vet, actionlint, fuzzing, vulnerability scan and benchmarks | Local checks passed; hosted CI/Windows runtime not executed here |
| Reconcile public safety claims with actual guarantees | Updated README, safety case, policy/audit/architecture guides, ADR 0003 and executable receiver example | Documentation updated; independent approval open |

Additional finding during verification: Linux `File.Fd` use raced device close
and allowed a descriptor-reuse window around poll/ioctl. `RawConn.Control` now
pins descriptor ownership for each syscall; 100 consecutive backend race runs
passed after the fix. Invalid raw readings are rejected, not clamped into live
intent. Driver calibration that legitimately overshoots must be corrected and
validated; the library does not invent a tolerance.

## Local verification record — 2026-09-05

Go 1.26.8, macOS/arm64 (Apple M1 Pro):

- `go test -race ./...`, `CGO_ENABLED=0 go test ./...`, `go vet ./...`: passed.
- Assured live-policy/receiver integration, including input supersession:
  10 consecutive race-instrumented repetitions passed.
- Full Linux/arm64 race test binaries built with Go 1.26.8 and Zig 0.14.0
  (`aarch64-linux-musl`) and executed in an offline Linux container: passed.
  These exercised Linux backend code; they were not merely cross-compiled.
- Windows/amd64 no-cgo test binaries: compilation passed. Windows runtime and
  physical controller/receiver hardware were not available for this validation.
- Coverage-guided fuzzing: Linux raw-axis backend 1,190,062 executions/20s;
  final strict command parser 231,882 executions/about 21s; audit verifier
  125,753 executions/about 21s. No failing case found. These bounded runs are
  supporting evidence, not exhaustive verification.
- `govulncheck ./...` (v1.5.0): **No vulnerabilities found** at scan time.
- Actionlint v1.7.12 (shellcheck/pyflakes disabled), `git diff --check` and
  `gofmt -l .`: passed. CI now includes pinned checkout/setup actions,
  vulnerability checking and three fuzz targets; no remote workflow was triggered.
- Native aggregate statement coverage: 78.2%; safety 87.3%, policy 83.7%,
  simulation 89.1%. This is not complete branch/path coverage, nor hardware
  evidence. Backend bindings requiring real devices remain untested here.

Benchmarks (`-benchtime=1s -count=3`) on the same development machine:

- Strict policy evaluation: 65.2–65.8 microseconds/operation, about 11.9 KB and
  280 allocations/operation.
- Local 4 KiB FileStore Write+Sync: 4.59–4.65 milliseconds/operation.

These are averages, not worst-case latency or crash-durability guarantees. A
single Apply requires multiple evidence barriers plus input traffic, signing
and receiver work. Installed-media tail latency under load/failure must fit the
approved lease/watchdog budget; do not infer that a 10 ms control loop is viable
from these numbers. Production Go processes are not hard-real-time systems.

## Compatibility changes

- `assured.Config.Authority.Policy` is now required. Legacy Assured
  configurations fail validation until a reviewed command policy is supplied.
- The scalar reference policy requires canonical float64 JSON and known
  acknowledged baselines; other command formats use a reviewed custom policy.
- Opening context cancellation ends startup, not an established Assured
  Session. Use `Session.Close` for ordered, evidenced shutdown.
- The module selects the patched Go 1.26.8 toolchain. Low-level authorities
  without a policy remain interlock-only, not a mission-critical bypass.
- Domain-neutral `safety.Command`, `StrictConfig`, `NewStrict`, and
  `assured.Config.Safety` retain the original maritime compatibility names.
  Dual nonzero profiles are rejected and existing audit wire fields are retained.
- Controller-only audit and game-shaped/vehicle receiver integrations verify
  the optional seams; the relevant race-instrumented tests passed ten repetitions.

Read [command policy and simulation](command-policy.md) and
[incident verification](audit.md#incident-verification) before integrating.

## Installation evidence required before mission-critical approval

- Vessel-specific hazard analysis and approved safe states for each operating mode.
- Authenticated operator grants and receiver session ownership; reviewed command
  mappings, units, limits and slew envelopes.
- Actual receiver/actuator adapter and independent hardware emergency stop,
  lease watchdog and authenticated physical feedback.
- Hardware-in-the-loop and sea-trial results for loss, delay, reordering,
  corruption, restart, power loss and equipment failure.
- Power-loss tests of the installed storage stack; independent witness identity,
  full evidence custody, key provisioning/rotation, retention and recovery drills.
- Independent safety/security review of the complete installed system.

These remain open until evidence is supplied and checked. Software simulations
are supporting evidence and do not close physical-system requirements.
