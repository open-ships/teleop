package teleop

import (
	"context"
	"crypto/rand"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"
)

// PipelineRequirements names the exact safety-relevant controller pipeline a
// caller expects a Provider to construct. Sinks and processors must be
// pointer-backed so their instance identity can be attested, not merely their
// concrete type. Their order and count are significant. All controller options
// not represented here are sealed to their conservative defaults: the system
// clock and background context, default buffers and callback timeout, eager
// start, fail-fast ingest overflow, no observation staleness policy, no stale
// neutralization, and the default clock-step threshold.
type PipelineRequirements struct {
	AuditSinks       []EventSink
	Processors       []Processor
	SynchronousAudit bool
	LivenessInterval time.Duration
	ShutdownTimeout  time.Duration
}

type pipelineCallbackIdentity struct {
	typeOf  reflect.Type
	pointer uintptr
}

type pipelineSnapshot struct {
	sinks              []pipelineCallbackIdentity
	processors         []pipelineCallbackIdentity
	defaultContext     bool
	systemClock        bool
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
	replayBackpressure bool
}

// PipelineAttestation is an opaque, per-open option seal. Construct one, pass
// Option as the last Provider open option, then call Controller.AttestPipeline
// on the concrete controller the Provider returns. A Provider cannot obtain a
// valid nonce by applying only a subset of the preceding options, and later
// changes to an attested setting invalidate verification.
type PipelineAttestation struct {
	mu               sync.Mutex
	nonce            [32]byte
	sinks            []pipelineCallbackIdentity
	processors       []pipelineCallbackIdentity
	synchronousAudit bool
	livenessInterval time.Duration
	shutdownTimeout  time.Duration
	sealed           *pipelineSnapshot
}

// NewPipelineAttestation snapshots requirements and creates a fresh option
// seal. A seal is single-use: create one for each open transaction.
func NewPipelineAttestation(requirements PipelineRequirements) (*PipelineAttestation, error) {
	if requirements.LivenessInterval < 0 {
		return nil, fmt.Errorf(
			"%w: negative liveness interval %s",
			ErrPipelineAttestation,
			requirements.LivenessInterval,
		)
	}
	if requirements.ShutdownTimeout <= 0 {
		return nil, fmt.Errorf(
			"%w: shutdown timeout must be positive",
			ErrPipelineAttestation,
		)
	}
	attestation := &PipelineAttestation{
		synchronousAudit: requirements.SynchronousAudit,
		livenessInterval: requirements.LivenessInterval,
		shutdownTimeout:  requirements.ShutdownTimeout,
	}
	var err error
	attestation.sinks, err = pipelineIdentities("audit sink", requirements.AuditSinks)
	if err != nil {
		return nil, err
	}
	attestation.processors, err = pipelineIdentities("processor", requirements.Processors)
	if err != nil {
		return nil, err
	}
	for attestation.nonce == ([32]byte{}) {
		if _, err := rand.Read(attestation.nonce[:]); err != nil {
			return nil, fmt.Errorf("%w: create nonce: %v", ErrPipelineAttestation, err)
		}
	}
	return attestation, nil
}

func pipelineIdentities[T any](label string, callbacks []T) ([]pipelineCallbackIdentity, error) {
	identities := make([]pipelineCallbackIdentity, len(callbacks))
	for index, callback := range callbacks {
		identity, ok := pipelineIdentity(any(callback))
		if !ok {
			return nil, fmt.Errorf(
				"%w: %s %d must be a non-nil pointer-backed instance",
				ErrPipelineAttestation,
				label,
				index,
			)
		}
		identities[index] = identity
	}
	return identities, nil
}

func pipelineIdentity(callback any) (pipelineCallbackIdentity, bool) {
	if callback == nil {
		return pipelineCallbackIdentity{}, false
	}
	value := reflect.ValueOf(callback)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return pipelineCallbackIdentity{}, false
	}
	return pipelineCallbackIdentity{
		typeOf:  value.Type(),
		pointer: value.Pointer(),
	}, true
}

// Option returns the single-use seal option. It must be applied after every
// other option. It snapshots the complete effective option state and stamps a
// nonce only when that state satisfies the requirements and conservative
// defaults. Applying it elsewhere cannot produce a second valid stamp.
func (attestation *PipelineAttestation) Option() OpenOption {
	return func(options *controllerOptions) {
		if options == nil {
			return
		}
		options.pipelineNonce = [32]byte{}
		if attestation == nil {
			return
		}
		snapshot, ok := snapshotPipeline(options)
		if !ok || !attestation.accepts(snapshot) {
			return
		}
		attestation.mu.Lock()
		defer attestation.mu.Unlock()
		if attestation.sealed != nil {
			return
		}
		attestation.sealed = &snapshot
		options.pipelineNonce = attestation.nonce
	}
}

// AttestPipeline verifies both the unforgeable option nonce and the
// controller's final effective settings. The second check catches a Provider
// that applies the seal and then appends an overriding option.
func (controller *Controller) AttestPipeline(attestation *PipelineAttestation) error {
	if controller == nil {
		return fmt.Errorf("%w: nil controller", ErrPipelineAttestation)
	}
	if attestation == nil {
		return fmt.Errorf("%w: nil attestation", ErrPipelineAttestation)
	}
	if controller.options.pipelineNonce != attestation.nonce {
		return fmt.Errorf("%w: option seal is absent or invalid", ErrPipelineAttestation)
	}
	current, ok := snapshotPipeline(&controller.options)
	if !ok {
		return fmt.Errorf("%w: effective pipeline identity is invalid", ErrPipelineAttestation)
	}
	attestation.mu.Lock()
	if attestation.sealed == nil {
		attestation.mu.Unlock()
		return fmt.Errorf("%w: option seal was not applied", ErrPipelineAttestation)
	}
	sealed := *attestation.sealed
	sealed.sinks = slices.Clone(sealed.sinks)
	sealed.processors = slices.Clone(sealed.processors)
	attestation.mu.Unlock()
	if !sealed.equal(current) || !attestation.accepts(current) {
		return fmt.Errorf("%w: effective pipeline changed after sealing", ErrPipelineAttestation)
	}
	return nil
}

func (attestation *PipelineAttestation) accepts(snapshot pipelineSnapshot) bool {
	return attestation != nil &&
		snapshot.defaultContext &&
		snapshot.systemClock &&
		snapshot.ingestBuffer == defaultIngestBuffer &&
		snapshot.sinkBuffer == defaultSinkBuffer &&
		snapshot.livenessInterval == attestation.livenessInterval &&
		snapshot.staleAfter == 0 &&
		snapshot.callbackTimeout == defaultCallbackTimeout &&
		snapshot.shutdownTimeout == attestation.shutdownTimeout &&
		snapshot.clockStepThreshold == 0 &&
		!snapshot.neutralizeOnStale &&
		snapshot.synchronousAudit == attestation.synchronousAudit &&
		!snapshot.deferredStart &&
		!snapshot.replayBackpressure &&
		slices.Equal(snapshot.sinks, attestation.sinks) &&
		slices.Equal(snapshot.processors, attestation.processors)
}

func snapshotPipeline(options *controllerOptions) (pipelineSnapshot, bool) {
	if options == nil {
		return pipelineSnapshot{}, false
	}
	snapshot := pipelineSnapshot{
		defaultContext:     isDefaultPipelineContext(options.context),
		ingestBuffer:       options.ingestBuffer,
		sinkBuffer:         options.sinkBuffer,
		livenessInterval:   options.livenessInterval,
		staleAfter:         options.staleAfter,
		callbackTimeout:    options.callbackTimeout,
		shutdownTimeout:    options.shutdownTimeout,
		clockStepThreshold: options.clockStepThreshold,
		neutralizeOnStale:  options.neutralizeOnStale,
		synchronousAudit:   options.synchronousAudit,
		deferredStart:      options.deferredStart,
		replayBackpressure: options.replayBackpressure,
	}
	_, snapshot.systemClock = options.clock.(systemClock)
	var ok bool
	snapshot.sinks, ok = snapshotPipelineIdentities(options.sinks)
	if !ok {
		return pipelineSnapshot{}, false
	}
	snapshot.processors, ok = snapshotPipelineIdentities(options.processors)
	if !ok {
		return pipelineSnapshot{}, false
	}
	return snapshot, true
}

func snapshotPipelineIdentities[T any](callbacks []T) ([]pipelineCallbackIdentity, bool) {
	identities := make([]pipelineCallbackIdentity, len(callbacks))
	for index, callback := range callbacks {
		identity, ok := pipelineIdentity(any(callback))
		if !ok {
			return nil, false
		}
		identities[index] = identity
	}
	return identities, true
}

func isDefaultPipelineContext(configured context.Context) bool {
	background := context.Background()
	return reflect.TypeOf(configured) == reflect.TypeOf(background) &&
		reflect.DeepEqual(configured, background)
}

func (snapshot pipelineSnapshot) equal(other pipelineSnapshot) bool {
	if snapshot.defaultContext != other.defaultContext ||
		snapshot.systemClock != other.systemClock ||
		snapshot.ingestBuffer != other.ingestBuffer ||
		snapshot.sinkBuffer != other.sinkBuffer ||
		snapshot.livenessInterval != other.livenessInterval ||
		snapshot.staleAfter != other.staleAfter ||
		snapshot.callbackTimeout != other.callbackTimeout ||
		snapshot.shutdownTimeout != other.shutdownTimeout ||
		snapshot.clockStepThreshold != other.clockStepThreshold ||
		snapshot.neutralizeOnStale != other.neutralizeOnStale ||
		snapshot.synchronousAudit != other.synchronousAudit ||
		snapshot.deferredStart != other.deferredStart ||
		snapshot.replayBackpressure != other.replayBackpressure {
		return false
	}
	return slices.Equal(snapshot.sinks, other.sinks) &&
		slices.Equal(snapshot.processors, other.processors)
}
