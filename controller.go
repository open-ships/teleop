package teleop

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

// DeliveryPolicy controls what happens when a subscription cannot keep up.
type DeliveryPolicy uint8

const (
	// DeliveryLossless terminates the subscription with
	// ErrSubscriptionOverflow instead of silently losing an event.
	DeliveryLossless DeliveryPolicy = iota
	// DeliveryLatest coalesces a full queue to the newest event and accounts for
	// every discarded event in SubscriptionStats and the authoritative stream.
	DeliveryLatest
)

// SubscriptionOptions configures a bounded event subscription.
type SubscriptionOptions struct {
	Delivery DeliveryPolicy
	Buffer   int
}

// SubscriptionStats is a snapshot of subscription delivery and loss.
type SubscriptionStats struct {
	Enqueued  uint64
	Delivered uint64
	Dropped   uint64
	Buffered  int
	Closed    bool
	Err       error
}

// Subscription is a bounded, context-aware controller event stream.
type Subscription interface {
	Next(context.Context) (Event, error)
	Stats() SubscriptionStats
	Close() error
}

// StateMeta describes the freshness and lifecycle of Snapshot.
type StateMeta struct {
	ObservedAt  time.Time
	ReceivedAt  time.Time
	PublishedAt time.Time

	// ReceivedMonotonic is the session-relative monotonic reading taken when
	// the latest observation arrived. Enforce a command timeout against this
	// and Controller.Monotonic rather than against ReceivedAt: a wall-clock
	// step can make stale input appear fresh.
	ReceivedMonotonic time.Duration

	Sequence  uint64
	Connected bool
	Stale     bool
	Synthetic bool
}

// GameController is an open controller session. Implementations are created by
// a Provider; the zero value is not usable.
type GameController interface {
	Descriptor() Descriptor
	Capabilities() Capabilities
	Snapshot() State
	SnapshotWithMeta() (State, StateMeta)
	Subscribe(SubscriptionOptions) (Subscription, error)
	RecordCommand(context.Context, Command) error
	Session() SessionID
	Done() <-chan struct{}
	Err() error
	Close() error
}

const (
	defaultIngestBuffer    = 512
	defaultSinkBuffer      = 2048
	defaultCallbackTimeout = 2 * time.Second
	defaultShutdownTimeout = time.Second
	advanceInterval        = 25 * time.Millisecond
)

type Controller struct {
	source     InputSource
	descriptor Descriptor
	options    controllerOptions
	session    SessionID
	clock      Clock

	sourceCtx    context.Context
	cancelSource context.CancelFunc
	pipelineCtx  context.Context
	cancelPipe   context.CancelFunc

	startOnce       sync.Once
	closeOnce       sync.Once
	sourceCloseOnce sync.Once
	done            chan struct{}
	ingest          chan sourceResult
	external        chan commandRequest
	fatal           chan error

	// clocks detects wall-clock steps. Only the run loop may call observe;
	// since is read-only and safe from any goroutine.
	clocks *clockMonitor

	stateMu sync.RWMutex
	state   State
	meta    StateMeta

	eventMu   sync.Mutex
	sequences map[string]uint64
	knownMu   sync.RWMutex
	known     map[EventID]struct{}

	subsMu       sync.Mutex
	subscribers  map[*eventSubscription]struct{}
	terminalErr  error
	connection   Event
	capabilities Event
	observation  Event

	sourceCloseMu  sync.Mutex
	sourceCloseErr error
	sourceClosed   chan struct{}

	sinks []*sinkRunner

	processorDisabled []bool
	lastLiveness      time.Time
	startedAt         time.Time
}

type sourceResult struct {
	observation Observation
	receivedAt  time.Time
	err         error
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
func (systemClock) NewTicker(interval time.Duration) Ticker {
	return systemTicker{Ticker: time.NewTicker(interval)}
}

type systemTicker struct{ *time.Ticker }

func (ticker systemTicker) C() <-chan time.Time { return ticker.Ticker.C }

// NewController opens the lifecycle around source and starts ingest
// immediately. An attached audit sink therefore records the session even when
// the caller never takes a snapshot or subscribes.
func NewController(source InputSource, options ...OpenOption) (*Controller, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: nil input source", ErrUnavailable)
	}
	configured := controllerOptions{
		context:         context.Background(),
		clock:           systemClock{},
		ingestBuffer:    defaultIngestBuffer,
		sinkBuffer:      defaultSinkBuffer,
		callbackTimeout: defaultCallbackTimeout,
		shutdownTimeout: defaultShutdownTimeout,
	}
	for _, option := range options {
		if option != nil {
			option(&configured)
		}
	}
	if configured.context == nil {
		configured.context = context.Background()
	}
	if configured.clock == nil {
		configured.clock = systemClock{}
	}
	sourceCtx, cancelSource := context.WithCancel(configured.context)
	pipelineCtx, cancelPipe := context.WithCancel(context.Background())
	controller := &Controller{
		source:       source,
		descriptor:   source.Descriptor().Clone(),
		options:      configured,
		clock:        configured.clock,
		sourceCtx:    sourceCtx,
		cancelSource: cancelSource,
		pipelineCtx:  pipelineCtx,
		cancelPipe:   cancelPipe,
		done:         make(chan struct{}),
		ingest:       make(chan sourceResult, configured.ingestBuffer),
		external:     make(chan commandRequest, configured.ingestBuffer),
		fatal:        make(chan error, len(configured.sinks)+2),
		clocks: newClockMonitor(
			configured.clock.Now(),
			configured.clockStepThreshold,
		),
		subscribers:  make(map[*eventSubscription]struct{}),
		sequences:    make(map[string]uint64),
		known:        make(map[EventID]struct{}),
		sourceClosed: make(chan struct{}),
		processorDisabled: make(
			[]bool,
			len(configured.processors),
		),
	}
	if _, err := rand.Read(controller.session[:]); err != nil {
		cancelSource()
		cancelPipe()
		_ = source.Close()
		return nil, fmt.Errorf("create controller session: %w", err)
	}
	for _, sink := range configured.sinks {
		runner := newSinkRunner(controller, sink, configured.sinkBuffer)
		controller.sinks = append(controller.sinks, runner)
		go runner.run()
	}
	if !configured.deferredStart {
		controller.start()
	}
	return controller, nil
}

func (c *Controller) start() {
	c.startOnce.Do(func() { go c.run() })
}

// Descriptor returns an isolated copy of the open device descriptor.
func (c *Controller) Descriptor() Descriptor { return c.descriptor.Clone() }

// Capabilities returns an isolated copy of the device capabilities.
func (c *Controller) Capabilities() Capabilities {
	return c.descriptor.Capability.Clone()
}

// Session returns the cryptographically random identity of this open session.
func (c *Controller) Session() SessionID { return c.session }

// Done closes after terminal disconnect and bounded sink/source cleanup has
// either completed or produced a terminal timeout error.
func (c *Controller) Done() <-chan struct{} { return c.done }

// Err returns the terminal controller error after Done closes, or nil while
// the controller is active.
func (c *Controller) Err() error {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	return c.terminalErr
}

// Snapshot returns the latest state. On stale input or disconnect it returns a
// synthesized neutral state.
func (c *Controller) Snapshot() State {
	state, _ := c.SnapshotWithMeta()
	return state
}

// SnapshotWithMeta returns the state and enough timing information to enforce
// a dead-man policy without relying on event delivery.
func (c *Controller) SnapshotWithMeta() (State, StateMeta) {
	c.start()
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state.Clone(), c.meta
}

// Subscribe attaches a bounded stream and immediately replays the latched
// connection, capabilities, and latest observation events.
func (c *Controller) Subscribe(options SubscriptionOptions) (Subscription, error) {
	if options.Buffer <= 0 {
		options.Buffer = 1024
	}
	if options.Delivery != DeliveryLossless && options.Delivery != DeliveryLatest {
		return nil, fmt.Errorf("%w: unknown delivery policy %d", ErrUnsupported, options.Delivery)
	}
	subscription := newEventSubscription(c, options.Delivery, options.Buffer)

	c.subsMu.Lock()
	if c.terminalErr != nil {
		err := c.terminalErr
		c.subsMu.Unlock()
		return nil, err
	}
	c.subscribers[subscription] = struct{}{}
	latched := []Event{c.connection, c.capabilities, c.observation}
	for _, event := range latched {
		if event == nil {
			continue
		}
		_, overflow := subscription.enqueue(cloneEvent(event))
		if overflow {
			delete(c.subscribers, subscription)
			break
		}
	}
	c.subsMu.Unlock()
	c.start()
	return subscription, nil
}

func (c *Controller) run() {
	defer close(c.done)
	defer c.cancelPipe()
	defer func() {
		if recovered := recover(); recovered != nil {
			c.terminate(fmt.Errorf("%w: controller pipeline: %v", ErrCallbackPanic, recovered))
		}
	}()
	go c.readLoop()

	now := c.clock.Now()
	c.startedAt = now
	c.stateMu.Lock()
	c.meta.Connected = true
	c.meta.PublishedAt = now
	c.stateMu.Unlock()
	if err := c.publish(ConnectionEvent{
		Meta:       c.nextHeaderAt("input", now, now, 0, nil, false),
		State:      Connected,
		Descriptor: c.Descriptor(),
	}); err != nil {
		c.terminate(err)
		return
	}
	if err := c.publish(CapabilitiesEvent{
		Meta:         c.nextHeaderAt("input", now, now, 0, nil, false),
		Capabilities: c.Capabilities(),
	}); err != nil {
		c.terminate(err)
		return
	}
	var ticker Ticker
	tickInterval := c.tickInterval()
	if tickInterval > 0 {
		ticker = c.clock.NewTicker(tickInterval)
		defer ticker.Stop()
	}
	var ticks <-chan time.Time
	if ticker != nil {
		ticks = ticker.C()
	}

	for {
		select {
		case result := <-c.ingest:
			if result.err != nil {
				c.terminate(normalizeSourceError(result.err, c.sourceCtx.Err() != nil))
				return
			}
			if err := c.handleObservation(result.observation, result.receivedAt); err != nil {
				c.terminate(err)
				return
			}
		case request := <-c.external:
			if err := c.handleCommand(request); err != nil {
				c.terminate(err)
				return
			}
		case err := <-c.fatal:
			c.terminate(err)
			return
		case now := <-ticks:
			if err := c.onTick(now); err != nil {
				c.terminate(err)
				return
			}
		case <-c.sourceCtx.Done():
			c.terminate(ErrClosed)
			return
		}
	}
}

func (c *Controller) readLoop() {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.requestFatal(fmt.Errorf("%w: input source read: %v", ErrCallbackPanic, recovered))
		}
	}()
	for {
		observation, err := c.source.Read(c.sourceCtx)
		result := sourceResult{
			observation: observation,
			receivedAt:  c.clock.Now(),
			err:         err,
		}
		select {
		case c.ingest <- result:
		case <-c.sourceCtx.Done():
			return
		default:
			c.requestFatal(ErrPipelineOverflow)
			return
		}
		if err != nil {
			return
		}
	}
}

func (c *Controller) handleObservation(observation Observation, receivedAt time.Time) error {
	if err := c.checkClock(receivedAt); err != nil {
		return err
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = receivedAt
	}
	state, changed := sanitizeState(observation.State)
	if changed {
		invalid := ErrorEvent{
			Meta: c.nextHeaderAt(
				"input",
				observation.ObservedAt,
				receivedAt,
				observation.DeviceTimestamp,
				nil,
				true,
			),
			Message: ErrInvalidState.Error() + ": non-finite or out-of-range values were neutralized",
			Err:     ErrInvalidState,
		}
		if err := c.publish(invalid); err != nil {
			return err
		}
	}
	if observation.Gap != nil {
		if err := c.publish(GapEvent{
			Meta: c.nextHeaderAt(
				"input",
				observation.ObservedAt,
				receivedAt,
				observation.DeviceTimestamp,
				nil,
				false,
			),
			Source:  c.descriptor.Backend,
			Dropped: observation.Gap.Dropped,
			Reason:  observation.Gap.Reason,
		}); err != nil {
			return err
		}
	}

	c.stateMu.Lock()
	previous := c.state.Clone()
	c.state = state.Clone()
	c.stateMu.Unlock()

	observationEvent := ObservationEvent{
		Meta: c.nextHeaderAt(
			"input",
			observation.ObservedAt,
			receivedAt,
			observation.DeviceTimestamp,
			nil,
			false,
		),
		Native:   observation.Native.clone(),
		Previous: previous,
		Current:  state,
	}
	if err := c.publish(observationEvent); err != nil {
		return err
	}
	cause := []EventID{observationEvent.Meta.ID}
	for _, event := range diffEvents(previous, state, func() Header {
		return c.nextHeaderAt(
			"input",
			observation.ObservedAt,
			receivedAt,
			observation.DeviceTimestamp,
			cause,
			false,
		)
	}) {
		if err := c.publish(event); err != nil {
			return err
		}
	}

	c.stateMu.Lock()
	c.meta = StateMeta{
		ObservedAt:        observation.ObservedAt,
		ReceivedAt:        receivedAt,
		PublishedAt:       observationEvent.Meta.PublishedAt,
		ReceivedMonotonic: observationEvent.Meta.ReceivedMonotonic,
		Sequence:          observationEvent.Meta.ID.Sequence,
		Connected:         true,
	}
	c.stateMu.Unlock()
	return nil
}

func (c *Controller) onTick(now time.Time) error {
	if err := c.checkClock(now); err != nil {
		return err
	}
	c.stateMu.RLock()
	meta := c.meta
	c.stateMu.RUnlock()
	if meta.ReceivedAt.IsZero() {
		meta.ReceivedAt = c.startedAt
	}
	age := now.Sub(meta.ReceivedAt)
	if age < 0 {
		age = 0
	}
	stale := c.options.staleAfter > 0 && age >= c.options.staleAfter
	if stale && !meta.Stale && c.options.neutralizeOnStale {
		if err := c.neutralize(now, "input stale"); err != nil {
			return err
		}
	}
	if c.options.livenessInterval > 0 &&
		(c.lastLiveness.IsZero() || now.Sub(c.lastLiveness) >= c.options.livenessInterval) {
		status := LivenessHealthy
		if stale {
			status = LivenessStale
		}
		if err := c.publish(LivenessEvent{
			Meta:         c.nextHeaderAt("input", now, now, 0, nil, true),
			State:        status,
			LastObserved: meta.ObservedAt,
			LastReceived: meta.ReceivedAt,
			Age:          age,
		}); err != nil {
			return err
		}
		c.lastLiveness = now
	}
	c.stateMu.Lock()
	c.meta.Stale = stale
	c.stateMu.Unlock()

	for index, processor := range c.options.processors {
		if err := c.advanceProcessor(index, processor, now); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) neutralize(now time.Time, reason string) error {
	c.stateMu.Lock()
	previous := c.state.Clone()
	c.state = State{}
	c.meta.Stale = true
	c.meta.Synthetic = true
	c.stateMu.Unlock()

	observation := ObservationEvent{
		Meta: c.nextHeaderAt("input", now, now, 0, nil, true),
		Native: NativeInput{
			Format: "teleop.synthetic." + reason,
		},
		Previous: previous,
		Current:  State{},
	}
	if err := c.publish(observation); err != nil {
		return err
	}
	cause := []EventID{observation.Meta.ID}
	for _, event := range diffEvents(previous, State{}, func() Header {
		return c.nextHeaderAt("input", now, now, 0, cause, true)
	}) {
		if err := c.publish(event); err != nil {
			return err
		}
	}
	c.stateMu.Lock()
	c.meta.PublishedAt = observation.Meta.PublishedAt
	c.meta.Sequence = observation.Meta.ID.Sequence
	c.stateMu.Unlock()
	return nil
}

// neutralizeTerminal guarantees canonical safety events first. Terminal
// processing skips failed stages, lets healthy stages close active state, and
// never lets one failed sink suppress subscriber delivery.
func (c *Controller) neutralizeTerminal(now time.Time, reason string) {
	c.stateMu.Lock()
	previous := c.state.Clone()
	c.state = State{}
	c.meta.Stale = true
	c.meta.Synthetic = true
	c.stateMu.Unlock()

	observation := ObservationEvent{
		Meta: c.nextHeaderAt("input", now, now, 0, nil, true),
		Native: NativeInput{
			Format: "teleop.synthetic." + reason,
		},
		Previous: previous,
		Current:  State{},
	}
	c.publishTerminal(observation)
	cause := []EventID{observation.Meta.ID}
	for _, event := range diffEvents(previous, State{}, func() Header {
		return c.nextHeaderAt("input", now, now, 0, cause, true)
	}) {
		c.publishTerminal(event)
	}
	c.stateMu.Lock()
	c.meta.PublishedAt = observation.Meta.PublishedAt
	c.meta.Sequence = observation.Meta.ID.Sequence
	c.stateMu.Unlock()
}

func (c *Controller) terminate(err error) {
	if err == nil {
		err = ErrDisconnected
	}
	now := c.clock.Now()
	c.neutralizeTerminal(now, "disconnect")

	if !errors.Is(err, ErrClosed) && !errors.Is(err, ErrDisconnected) && !errors.Is(err, io.EOF) {
		c.publishTerminal(ErrorEvent{
			Meta:    c.nextHeaderAt("input", now, now, 0, nil, true),
			Message: err.Error(),
			Err:     err,
		})
	}
	reason := err.Error()
	c.stateMu.Lock()
	c.meta.Connected = false
	c.meta.Stale = true
	c.meta.Synthetic = true
	c.meta.PublishedAt = now
	c.stateMu.Unlock()
	c.publishTerminal(ConnectionEvent{
		Meta:       c.nextHeaderAt("input", now, now, 0, nil, true),
		State:      Disconnected,
		Reason:     reason,
		Descriptor: c.Descriptor(),
	})

	c.cancelSource()
	go c.closeSource()
	c.finishSubscriptions(err)
	deadline := time.Now().Add(c.options.shutdownTimeout)
	c.addTerminalError(c.stopSinks(deadline))
	c.addTerminalError(c.waitSourceClose(deadline))
}

func normalizeSourceError(err error, closing bool) error {
	if closing || errors.Is(err, context.Canceled) || errors.Is(err, ErrClosed) {
		return ErrClosed
	}
	if errors.Is(err, io.EOF) || errors.Is(err, ErrDisconnected) {
		return ErrDisconnected
	}
	return errors.Join(ErrDisconnected, err)
}

func (c *Controller) tickInterval() time.Duration {
	interval := time.Duration(0)
	if c.options.livenessInterval > 0 {
		interval = c.options.livenessInterval
	}
	if c.options.staleAfter > 0 &&
		(interval == 0 || c.options.staleAfter < interval) {
		interval = c.options.staleAfter
	}
	for _, processor := range c.options.processors {
		_, contextual := processor.(ContextAdvancingProcessor)
		_, legacy := processor.(AdvancingProcessor)
		if (contextual || legacy) && (interval == 0 || advanceInterval < interval) {
			interval = advanceInterval
		}
	}
	return interval
}

func (c *Controller) nextHeaderAt(
	stream string,
	observedAt time.Time,
	receivedAt time.Time,
	deviceTimestamp int64,
	causes []EventID,
	synthetic bool,
) Header {
	c.eventMu.Lock()
	c.sequences[stream]++
	sequence := c.sequences[stream]
	c.eventMu.Unlock()
	if receivedAt.IsZero() {
		receivedAt = c.clock.Now()
	}
	publishedAt := c.clock.Now()
	return Header{
		ID: EventID{
			Session:  c.session,
			Stream:   stream,
			Sequence: sequence,
		},
		DeviceID:          c.descriptor.ID,
		ObservedAt:        observedAt,
		ReceivedAt:        receivedAt,
		PublishedAt:       publishedAt,
		Monotonic:         c.clocks.since(publishedAt),
		ReceivedMonotonic: c.clocks.since(receivedAt),
		DeviceTimestamp:   deviceTimestamp,
		Causes:            append([]EventID(nil), causes...),
		Synthetic:         synthetic,
	}
}

// checkClock publishes a ClockEvent when the host wall clock has stepped
// relative to the monotonic clock. Callers must be the run loop: clockMonitor
// state is not concurrency safe.
func (c *Controller) checkClock(now time.Time) error {
	sample := c.clocks.observe(now)
	if !sample.Stepped {
		return nil
	}
	return c.publish(ClockEvent{
		Meta:   c.nextHeaderAt("input", now, now, 0, nil, true),
		Step:   sample.Step,
		Steps:  c.clocks.stepCount(),
		Reason: "host wall clock stepped relative to monotonic clock",
	})
}

// NewHeader implements ProcessingContext.
func (c *Controller) NewHeader(
	stream string,
	observedAt time.Time,
	deviceTimestamp int64,
	causes ...EventID,
) Header {
	if stream == "" {
		stream = "derived"
	}
	return c.nextHeaderAt(
		stream,
		observedAt,
		c.clock.Now(),
		deviceTimestamp,
		causes,
		false,
	)
}

// Now implements ProcessingContext.
func (c *Controller) Now() time.Time { return c.clock.Now() }

// Monotonic returns the controller's current session-relative monotonic
// reading. Subtracting StateMeta.ReceivedMonotonic from it yields an input age
// that a wall-clock adjustment cannot falsify, which is the measurement a
// command timeout must be built on.
func (c *Controller) Monotonic() time.Duration {
	return c.clocks.since(c.clock.Now())
}

func (c *Controller) publish(event Event) error {
	if event == nil {
		return nil
	}
	if err := c.dispatch(event, true); err != nil {
		return err
	}
	return c.processStages([]Event{event}, 0)
}

func (c *Controller) publishTerminal(event Event) {
	if event == nil || !c.dispatchTerminal(event) {
		return
	}
	visible := []Event{event}
	for index, processor := range c.options.processors {
		if c.processorDisabled[index] {
			continue
		}
		stageInputs := append([]Event(nil), visible...)
		var derived []Event
		failed := false
		for _, input := range stageInputs {
			output, err := c.callProcessor(processor, input)
			if err != nil {
				c.processorDisabled[index] = true
				failed = true
				break
			}
			derived = append(derived, output...)
		}
		if failed {
			continue
		}
		for _, output := range derived {
			if output == nil {
				continue
			}
			if !c.dispatchTerminal(output) {
				c.processorDisabled[index] = true
				break
			}
			visible = append(visible, output)
		}
	}
}

func (c *Controller) dispatchTerminal(event Event) (completed bool) {
	defer func() {
		if recover() != nil {
			completed = false
		}
	}()
	_ = c.dispatch(event, false)
	return true
}

func (c *Controller) processStages(inputs []Event, start int) error {
	visible := append([]Event(nil), inputs...)
	for index := start; index < len(c.options.processors); index++ {
		if c.processorDisabled[index] {
			continue
		}
		processor := c.options.processors[index]
		stageInputs := append([]Event(nil), visible...)
		derived := make([]Event, 0, len(stageInputs))
		for _, input := range stageInputs {
			output, err := c.callProcessor(processor, input)
			if err != nil {
				c.processorDisabled[index] = true
				return err
			}
			derived = append(derived, output...)
		}
		for _, output := range derived {
			if output == nil {
				continue
			}
			if err, panicked := c.dispatchProcessorOutput(output, true); err != nil {
				if panicked {
					c.processorDisabled[index] = true
				}
				return err
			}
			visible = append(visible, output)
		}
	}
	return nil
}

func (c *Controller) advanceProcessor(index int, processor Processor, now time.Time) error {
	if c.processorDisabled[index] {
		return nil
	}
	var (
		derived []Event
		err     error
	)
	if contextual, ok := processor.(ContextAdvancingProcessor); ok {
		derived, err = callGuarded(c, func(ctx context.Context) ([]Event, error) {
			return contextual.AdvanceContext(ctx, c, now)
		})
	} else if advancing, ok := processor.(AdvancingProcessor); ok {
		derived, err = callGuarded(c, func(context.Context) ([]Event, error) {
			return advancing.Advance(now), nil
		})
	} else {
		return nil
	}
	if err != nil {
		c.processorDisabled[index] = true
		return err
	}
	for _, event := range derived {
		if event == nil {
			continue
		}
		if err, panicked := c.dispatchProcessorOutput(event, true); err != nil {
			if panicked {
				c.processorDisabled[index] = true
			}
			return err
		}
	}
	return c.processStages(derived, index+1)
}

func (c *Controller) dispatchProcessorOutput(
	event Event,
	reportLoss bool,
) (err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: processor event: %v", ErrCallbackPanic, recovered)
			panicked = true
		}
	}()
	return c.dispatch(event, reportLoss), false
}

func (c *Controller) callProcessor(processor Processor, event Event) ([]Event, error) {
	return callGuarded(c, func(ctx context.Context) ([]Event, error) {
		if contextual, ok := processor.(ContextProcessor); ok {
			return contextual.ProcessContext(ctx, c, cloneEvent(event))
		}
		return processor.Process(cloneEvent(event)), nil
	})
}

func callGuarded(
	c *Controller,
	callback func(context.Context) ([]Event, error),
) (events []Event, err error) {
	ctx, cancel := context.WithTimeout(c.pipelineCtx, c.options.callbackTimeout)
	defer cancel()
	result := make(chan struct {
		events []Event
		err    error
	}, 1)
	go func() {
		var outcome struct {
			events []Event
			err    error
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				outcome.err = fmt.Errorf("%w: %v", ErrCallbackPanic, recovered)
			}
			result <- outcome
		}()
		outcome.events, outcome.err = callback(ctx)
	}()
	select {
	case outcome := <-result:
		return outcome.events, outcome.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrCallbackTimeout
		}
		return nil, ErrClosed
	}
}

func (c *Controller) dispatch(event Event, reportLoss bool) error {
	dispatchErr := c.enqueueSinks(event)
	if dispatchErr == nil {
		// Record identity before subscribers can observe the event. This makes a
		// command causally valid exactly when its caller can have received the
		// event ID, without retaining mutable event payloads in the controller.
		c.knownMu.Lock()
		c.known[event.Header().ID] = struct{}{}
		c.knownMu.Unlock()
	}

	c.subsMu.Lock()
	switch event.Kind() {
	case EventConnection:
		c.connection = cloneEvent(event)
	case EventCapabilities:
		c.capabilities = cloneEvent(event)
	case EventObservation:
		c.observation = cloneEvent(event)
	}
	subscribers := make([]*eventSubscription, 0, len(c.subscribers))
	for subscriber := range c.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	c.subsMu.Unlock()

	var latestDropped, losslessDropped uint64
	overflowed := make([]*eventSubscription, 0)
	for _, subscriber := range subscribers {
		lost, overflow := subscriber.enqueue(cloneEvent(event))
		if overflow {
			losslessDropped += lost
			overflowed = append(overflowed, subscriber)
		} else {
			latestDropped += lost
		}
	}
	if len(overflowed) > 0 {
		c.subsMu.Lock()
		for _, subscriber := range overflowed {
			delete(c.subscribers, subscriber)
		}
		c.subsMu.Unlock()
	}
	if reportLoss {
		if latestDropped > 0 {
			dispatchErr = errors.Join(
				dispatchErr,
				c.recordSubscriptionGap(
					event,
					latestDropped,
					"DeliveryLatest coalesced a full queue",
				),
			)
		}
		if losslessDropped > 0 {
			dispatchErr = errors.Join(
				dispatchErr,
				c.recordSubscriptionGap(
					event,
					losslessDropped,
					"DeliveryLossless subscription overflowed",
				),
			)
		}
	}
	return dispatchErr
}

func (c *Controller) enqueueSinks(event Event) error {
	var result error
	for _, sink := range c.sinks {
		if err := sink.enqueue(cloneEvent(event)); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (c *Controller) recordSubscriptionGap(
	cause Event,
	dropped uint64,
	reason string,
) error {
	now := c.clock.Now()
	return c.enqueueSinks(GapEvent{
		Meta:    c.nextHeaderAt("input", now, now, 0, []EventID{cause.Header().ID}, true),
		Source:  "subscription",
		Dropped: dropped,
		Reason:  reason,
	})
}

func (c *Controller) requestFatal(err error) {
	if err == nil {
		return
	}
	select {
	case c.fatal <- err:
	default:
	}
}

func (c *Controller) finishSubscriptions(err error) {
	c.subsMu.Lock()
	if c.terminalErr == nil {
		c.terminalErr = err
	}
	subscribers := make([]*eventSubscription, 0, len(c.subscribers))
	for subscriber := range c.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	c.subscribers = make(map[*eventSubscription]struct{})
	c.subsMu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.terminate(err)
	}
}

func (c *Controller) addTerminalError(err error) {
	if err == nil {
		return
	}
	c.subsMu.Lock()
	c.terminalErr = errors.Join(c.terminalErr, err)
	c.subsMu.Unlock()
}

func (c *Controller) removeSubscription(subscription *eventSubscription) {
	c.subsMu.Lock()
	delete(c.subscribers, subscription)
	c.subsMu.Unlock()
}

func (c *Controller) closeSource() {
	c.sourceCloseOnce.Do(func() {
		err := func() (err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = fmt.Errorf("%w: input source close: %v", ErrCallbackPanic, recovered)
				}
			}()
			return c.source.Close()
		}()
		c.sourceCloseMu.Lock()
		c.sourceCloseErr = err
		c.sourceCloseMu.Unlock()
		close(c.sourceClosed)
	})
}

func (c *Controller) stopSinks(deadline time.Time) error {
	for _, sink := range c.sinks {
		sink.stop()
	}
	var result error
	for _, sink := range c.sinks {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			result = errors.Join(result, fmt.Errorf("%w: drain event sinks", ErrCallbackTimeout))
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case <-sink.done:
		case <-timer.C:
			result = errors.Join(result, fmt.Errorf("%w: drain event sinks", ErrCallbackTimeout))
			return result
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		result = errors.Join(result, sink.Err())
	}
	return result
}

func (c *Controller) waitSourceClose(deadline time.Time) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("%w: close input source", ErrCallbackTimeout)
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-c.sourceClosed:
		c.sourceCloseMu.Lock()
		defer c.sourceCloseMu.Unlock()
		return c.sourceCloseErr
	case <-timer.C:
		return fmt.Errorf("%w: close input source", ErrCallbackTimeout)
	}
}

// Close requests a neutralizing disconnect and waits for bounded cleanup. It
// returns the terminal pipeline or source-close error when one occurred.
func (c *Controller) Close() error {
	c.closeOnce.Do(func() {
		c.start()
		c.cancelSource()
		go c.closeSource()
	})
	timer := time.NewTimer(c.options.callbackTimeout + c.options.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
		return ErrCallbackTimeout
	}

	err := c.Err()
	if err != nil && err != ErrClosed {
		return err
	}
	return nil
}

type sinkRunner struct {
	controller *Controller
	sink       EventSink
	queue      chan Event
	done       chan struct{}
	stopOnce   sync.Once

	mu  sync.Mutex
	err error
}

func newSinkRunner(controller *Controller, sink EventSink, buffer int) *sinkRunner {
	return &sinkRunner{
		controller: controller,
		sink:       sink,
		queue:      make(chan Event, buffer),
		done:       make(chan struct{}),
	}
}

func (runner *sinkRunner) run() {
	defer close(runner.done)
	for event := range runner.queue {
		if err := runner.record(event); err != nil {
			runner.mu.Lock()
			runner.err = err
			runner.mu.Unlock()
			runner.controller.requestFatal(fmt.Errorf("record controller event: %w", err))
			return
		}
	}
}

func (runner *sinkRunner) record(event Event) error {
	ctx, cancel := context.WithTimeout(
		runner.controller.pipelineCtx,
		runner.controller.options.callbackTimeout,
	)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("%w: %v", ErrCallbackPanic, recovered)
			}
			result <- err
		}()
		err = runner.sink.Record(ctx, event)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrCallbackTimeout
		}
		return ErrClosed
	}
}

func (runner *sinkRunner) enqueue(event Event) error {
	runner.mu.Lock()
	err := runner.err
	runner.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case runner.queue <- event:
		return nil
	default:
		return ErrPipelineOverflow
	}
}

func (runner *sinkRunner) stop() {
	runner.stopOnce.Do(func() { close(runner.queue) })
}

func (runner *sinkRunner) Err() error {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.err
}

type eventSubscription struct {
	controller *Controller
	policy     DeliveryPolicy
	limit      int

	mu        sync.Mutex
	queue     []Event
	head      int
	size      int
	notify    chan struct{}
	closed    bool
	err       error
	enqueued  uint64
	delivered uint64
	dropped   uint64
}

func newEventSubscription(
	controller *Controller,
	policy DeliveryPolicy,
	limit int,
) *eventSubscription {
	return &eventSubscription{
		controller: controller,
		policy:     policy,
		limit:      limit,
		queue:      make([]Event, limit),
		notify:     make(chan struct{}),
	}
}

func (s *eventSubscription) enqueue(event Event) (dropped uint64, fatal bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, false
	}
	if s.size == s.limit {
		if s.policy == DeliveryLatest {
			dropped = uint64(s.size)
			s.dropped += dropped
			for index := range s.queue {
				s.queue[index] = nil
			}
			s.head = 0
			s.size = 0
		} else {
			s.closed = true
			s.err = ErrSubscriptionOverflow
			s.dropped++
			s.broadcastLocked()
			return 1, true
		}
	}
	index := (s.head + s.size) % s.limit
	s.queue[index] = event
	s.size++
	s.enqueued++
	s.broadcastLocked()
	return dropped, false
}

func (s *eventSubscription) broadcastLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

func (s *eventSubscription) Next(ctx context.Context) (Event, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.size > 0 {
			event := s.queue[s.head]
			s.queue[s.head] = nil
			s.head = (s.head + 1) % s.limit
			s.size--
			s.delivered++
			s.mu.Unlock()
			return event, nil
		}
		if s.closed {
			err := s.err
			if err == nil {
				err = ErrClosed
			}
			s.mu.Unlock()
			return nil, err
		}
		notify := s.notify
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-notify:
		}
	}
}

func (s *eventSubscription) Stats() SubscriptionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SubscriptionStats{
		Enqueued:  s.enqueued,
		Delivered: s.delivered,
		Dropped:   s.dropped,
		Buffered:  s.size,
		Closed:    s.closed,
		Err:       s.err,
	}
}

func (s *eventSubscription) terminate(err error) {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.err = err
	}
	s.broadcastLocked()
	s.mu.Unlock()
}

func (s *eventSubscription) Close() error {
	s.terminate(ErrClosed)
	s.controller.removeSubscription(s)
	return nil
}

func sanitizeState(state State) (State, bool) {
	state = state.Clone()
	changed := false
	sanitize := func(value, minimum, maximum float32) float32 {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			changed = true
			return 0
		}
		clamped := Clamp(value, minimum, maximum)
		if clamped != value {
			changed = true
		}
		return clamped
	}
	state.LeftStick.X = sanitize(state.LeftStick.X, -1, 1)
	state.LeftStick.Y = sanitize(state.LeftStick.Y, -1, 1)
	state.RightStick.X = sanitize(state.RightStick.X, -1, 1)
	state.RightStick.Y = sanitize(state.RightStick.Y, -1, 1)
	state.LeftTrigger = sanitize(state.LeftTrigger, 0, 1)
	state.RightTrigger = sanitize(state.RightTrigger, 0, 1)
	for id, pressed := range state.Buttons.Extensions {
		if !pressed {
			delete(state.Buttons.Extensions, id)
			changed = true
		}
	}
	return state, changed
}
