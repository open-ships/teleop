package teleop

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrClosed reports an explicitly closed controller or subscription.
	ErrClosed = errors.New("teleop: controller closed")
	// ErrDisconnected reports that the device transport was lost.
	ErrDisconnected = errors.New("teleop: controller disconnected")
	// ErrUnsupported reports an operation unavailable on this platform or device.
	ErrUnsupported = errors.New("teleop: unsupported")
	// ErrPermission reports insufficient permission to access a device.
	ErrPermission = errors.New("teleop: permission denied")
	// ErrUnavailable reports that a requested device or resource is unavailable.
	ErrUnavailable = errors.New("teleop: unavailable")
	// ErrSubscriptionOverflow reports loss on a lossless subscription.
	ErrSubscriptionOverflow = errors.New("teleop: subscription overflow")
	// ErrPipelineOverflow reports that a bounded controller pipeline could not
	// keep pace with the device stream.
	ErrPipelineOverflow = errors.New("teleop: pipeline overflow")
	// ErrPipelineAttestation reports that a controller's effective callbacks
	// and safety-relevant settings do not match an option seal supplied at open.
	ErrPipelineAttestation = errors.New("teleop: controller pipeline attestation failed")
	// ErrCallbackPanic reports a panic recovered from an InputSource, EventSink,
	// Processor, or third-party event implementation.
	ErrCallbackPanic = errors.New("teleop: callback panic")
	// ErrCallbackTimeout reports a callback that did not return before its
	// configured deadline.
	ErrCallbackTimeout = errors.New("teleop: callback timeout")
	// ErrInvalidState reports a non-finite or out-of-range backend state.
	ErrInvalidState = errors.New("teleop: invalid controller state")
	// ErrCommandPublicationUncertain reports that a synchronous command crossed
	// queue admission but its caller stopped waiting, or that a later processor
	// failed after the command itself became visible. The command may be present
	// in subscriptions and evidence; callers must reconcile by EventID or treat
	// the outcome as a safety fault rather than retrying it as definitely absent.
	ErrCommandPublicationUncertain = errors.New("teleop: command publication outcome uncertain")
)

// SourceGap describes input known or suspected to be missing before an
// observation.
type SourceGap struct {
	// Dropped is the known count, or a lower bound when Reason identifies a
	// loss signal that cannot report its exact magnitude. Zero means unknown.
	Dropped uint64
	Reason  string
}

// Observation is one complete state reading received from an OS backend.
// Native contains the closest practical representation of the source reading.
type Observation struct {
	State           State
	ObservedAt      time.Time
	DeviceTimestamp int64
	Native          NativeInput
	Gap             *SourceGap
}

// InputSource is the small boundary implemented by platform drivers and fakes.
type InputSource interface {
	Descriptor() Descriptor
	Read(context.Context) (Observation, error)
	Close() error
}

// EventSink receives the authoritative ordered controller event stream.
type EventSink interface {
	Record(context.Context, Event) error
}

// ProcessingContext supplies controller-owned identity and timing to a
// processor. Using NewHeader prevents derived event ID collisions when
// multiple processor instances are composed.
type ProcessingContext interface {
	NewHeader(stream string, observedAt time.Time, deviceTimestamp int64, causes ...EventID) Header
	Now() time.Time
}

// Processor derives events from events earlier in a controller pipeline.
// Processors are applied in option order; a later processor sees canonical
// events and output from all earlier processors.
type Processor interface {
	Process(Event) []Event
}

// ContextProcessor is the preferred processor contract. Legacy Processor
// implementations remain supported, but cannot use the controller's identity
// allocator or deterministic clock.
type ContextProcessor interface {
	Processor
	ProcessContext(context.Context, ProcessingContext, Event) ([]Event, error)
}

// AdvancingProcessor emits time-based events even when the controller is
// otherwise idle. Controller invokes it on a short internal ticker.
type AdvancingProcessor interface {
	Processor
	Advance(time.Time) []Event
}

// ContextAdvancingProcessor is the deterministic, cancellable form of
// AdvancingProcessor.
type ContextAdvancingProcessor interface {
	ContextProcessor
	AdvanceContext(context.Context, ProcessingContext, time.Time) ([]Event, error)
}

// Ticker is the clock seam used by Controller for liveness and advancing
// processors.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock provides deterministic controller time in tests and replay.
type Clock interface {
	Now() time.Time
	NewTicker(time.Duration) Ticker
}

type controllerOptions struct {
	sinks              []EventSink
	processors         []Processor
	context            context.Context
	clock              Clock
	ingestBuffer       int
	sinkBuffer         int
	livenessInterval   time.Duration
	staleAfter         time.Duration
	callbackTimeout    time.Duration
	shutdownTimeout    time.Duration
	clockStepThreshold time.Duration
	neutralizeOnStale  bool
	synchronousAudit   bool
	deferredStart      bool
	pipelineNonce      [32]byte
}

// OpenOption configures a controller session.
type OpenOption func(*controllerOptions)

// WithAuditSink attaches an authoritative ingress sink. The controller accepts
// each event into every bounded sink queue before publishing it to
// subscriptions; sink I/O runs independently of ordinary device ingest. A
// CanonicalEventSink receives immutable bytes captured before queue handoff.
// RecordCommandSync provides the explicit callback-completion barrier when an
// application must establish sink durability before actuation.
func WithAuditSink(sink EventSink) OpenOption {
	return func(options *controllerOptions) {
		if sink != nil {
			options.sinks = append(options.sinks, sink)
		}
	}
}

// WithSynchronousAudit requires every event to complete every configured audit
// sink callback before the controller commits corresponding snapshot state,
// exposes the event to subscribers, or runs processors. It converts sink
// latency into controller latency and can therefore trip configured deadlines;
// use it when evidence completeness is more important than decoupled ingest.
//
// Callback completion is only as strong as each EventSink contract. Pair this
// option with a sync-capable recorder when local crash durability is required.
func WithSynchronousAudit() OpenOption {
	return func(options *controllerOptions) {
		options.synchronousAudit = true
	}
}

// WithProcessor adds an ordered derived-event stage. A typical pipeline adds a
// gesture recognizer followed by an action mapper.
func WithProcessor(processor Processor) OpenOption {
	return func(options *controllerOptions) {
		if processor != nil {
			options.processors = append(options.processors, processor)
		}
	}
}

// WithContext binds the controller lifetime to ctx.
func WithContext(ctx context.Context) OpenOption {
	return func(options *controllerOptions) {
		if ctx != nil {
			options.context = ctx
		}
	}
}

// WithDeferredStart waits to start device ingest until the first Snapshot or
// Subscribe call. It is useful for finite replay sources, whose complete event
// history must not finish before a subscriber is attached. Audit-only sessions
// should use the default eager start.
func WithDeferredStart() OpenOption {
	return func(options *controllerOptions) {
		options.deferredStart = true
	}
}

// WithClock replaces wall-clock time and tickers. It is primarily intended for
// deterministic replay and tests.
func WithClock(clock Clock) OpenOption {
	return func(options *controllerOptions) {
		if clock != nil {
			options.clock = clock
		}
	}
}

// WithPipelineBuffers sets the bounded source-ingest and per-sink queue sizes.
func WithPipelineBuffers(ingest, sink int) OpenOption {
	return func(options *controllerOptions) {
		if ingest > 0 {
			options.ingestBuffer = ingest
		}
		if sink > 0 {
			options.sinkBuffer = sink
		}
	}
}

// WithLiveness configures observation-age events and the age at which metadata
// is marked stale. Set interval to zero to disable heartbeat events; freshness
// metadata is still checked at staleAfter. Change-driven backends emit nothing
// while a control is held steady, so age alone does not prove a transport
// failure and does not neutralize state unless WithNeutralizeOnStale(true) is
// also supplied.
func WithLiveness(interval, staleAfter time.Duration) OpenOption {
	return func(options *controllerOptions) {
		if interval < 0 || staleAfter < 0 {
			panic(fmt.Sprintf("teleop: negative liveness duration: %s, %s", interval, staleAfter))
		}
		options.livenessInterval = interval
		options.staleAfter = staleAfter
	}
}

// WithNeutralizeOnStale opts into synthesizing a neutral observation when the
// configured observation-age threshold is exceeded. Applications should use
// this only when silence is known to indicate transport failure for their
// source. Disconnects are always neutralized.
func WithNeutralizeOnStale(enabled bool) OpenOption {
	return func(options *controllerOptions) {
		options.neutralizeOnStale = enabled
	}
}

// WithCallbackTimeout bounds Processor and EventSink calls.
func WithCallbackTimeout(timeout time.Duration) OpenOption {
	return func(options *controllerOptions) {
		if timeout > 0 {
			options.callbackTimeout = timeout
		}
	}
}

// WithClockStepThreshold sets the wall-versus-monotonic divergence reported as
// a ClockEvent. It defaults to DefaultClockStepThreshold. Wall-clock
// timestamps spanning a step are not comparable; header monotonic readings
// remain valid across one.
func WithClockStepThreshold(threshold time.Duration) OpenOption {
	return func(options *controllerOptions) {
		if threshold > 0 {
			options.clockStepThreshold = threshold
		}
	}
}

// WithShutdownTimeout bounds terminal sink draining and source closure after
// any in-flight callback has reached its separately configured callback
// timeout.
func WithShutdownTimeout(timeout time.Duration) OpenOption {
	return func(options *controllerOptions) {
		if timeout > 0 {
			options.shutdownTimeout = timeout
		}
	}
}
