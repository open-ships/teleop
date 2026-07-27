package teleop

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

type DeliveryPolicy uint8

const (
	// DeliveryLossless terminates the subscription with
	// ErrSubscriptionOverflow instead of silently losing an event.
	DeliveryLossless DeliveryPolicy = iota
	// DeliveryLatest discards the oldest queued event when its buffer is full.
	// It is intended for monitors and other snapshot-oriented consumers.
	DeliveryLatest
)

type SubscriptionOptions struct {
	Delivery DeliveryPolicy
	Buffer   int
}

type Subscription interface {
	Next(context.Context) (Event, error)
	Close() error
}

type GameController interface {
	Descriptor() Descriptor
	Capabilities() Capabilities
	Snapshot() State
	Subscribe(SubscriptionOptions) (Subscription, error)
	Close() error
}

type Controller struct {
	source     InputSource
	descriptor Descriptor
	options    controllerOptions
	session    SessionID

	ctx    context.Context
	cancel context.CancelFunc

	startOnce sync.Once
	closeOnce sync.Once
	done      chan struct{}

	stateMu sync.RWMutex
	state   State

	pipelineMu sync.Mutex
	eventMu    sync.Mutex
	sequence   uint64

	subsMu      sync.Mutex
	subscribers map[*eventSubscription]struct{}
	terminalErr error

	backgroundMu  sync.Mutex
	backgroundErr error
}

func NewController(source InputSource, options ...OpenOption) (*Controller, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: nil input source", ErrUnavailable)
	}
	var configured controllerOptions
	for _, option := range options {
		if option != nil {
			option(&configured)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	controller := &Controller{
		source:      source,
		descriptor:  source.Descriptor().Clone(),
		options:     configured,
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
		subscribers: make(map[*eventSubscription]struct{}),
	}
	if _, err := rand.Read(controller.session[:]); err != nil {
		cancel()
		_ = source.Close()
		return nil, fmt.Errorf("create controller session: %w", err)
	}
	return controller, nil
}

func (c *Controller) Descriptor() Descriptor {
	return c.descriptor.Clone()
}

func (c *Controller) Capabilities() Capabilities {
	return c.descriptor.Capability.Clone()
}

func (c *Controller) Snapshot() State {
	c.start()
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state.Clone()
}

func (c *Controller) Subscribe(options SubscriptionOptions) (Subscription, error) {
	if options.Buffer <= 0 {
		options.Buffer = 1024
	}
	subscription := &eventSubscription{
		controller: c,
		policy:     options.Delivery,
		limit:      options.Buffer,
		notify:     make(chan struct{}, 1),
	}

	c.subsMu.Lock()
	if c.terminalErr != nil {
		err := c.terminalErr
		c.subsMu.Unlock()
		return nil, err
	}
	c.subscribers[subscription] = struct{}{}
	c.subsMu.Unlock()
	c.start()
	return subscription, nil
}

func (c *Controller) start() {
	c.startOnce.Do(func() {
		go c.run()
	})
}

func (c *Controller) run() {
	defer close(c.done)
	defer func() { _ = c.source.Close() }()
	advanceDone := make(chan struct{})
	go c.advanceLoop(advanceDone)
	defer func() {
		c.cancel()
		<-advanceDone
	}()

	if c.ctx.Err() != nil {
		c.finish(ErrClosed)
		return
	}
	if err := c.publish(ConnectionEvent{
		Meta:       c.nextHeader(time.Now(), 0, nil),
		State:      Connected,
		Descriptor: c.Descriptor(),
	}); err != nil {
		c.finish(err)
		return
	}
	if err := c.publish(CapabilitiesEvent{
		Meta:         c.nextHeader(time.Now(), 0, nil),
		Capabilities: c.Capabilities(),
	}); err != nil {
		c.finish(err)
		return
	}

	for {
		observation, err := c.source.Read(c.ctx)
		if err != nil {
			if c.ctx.Err() != nil || errors.Is(err, ErrClosed) {
				terminalErr := ErrClosed
				reason := "controller closed"
				if backgroundErr := c.getBackgroundError(); backgroundErr != nil {
					terminalErr = backgroundErr
					reason = backgroundErr.Error()
				}
				_ = c.publish(ConnectionEvent{
					Meta:       c.nextHeader(time.Now(), 0, nil),
					State:      Disconnected,
					Reason:     reason,
					Descriptor: c.Descriptor(),
				})
				c.finish(terminalErr)
				return
			}
			reason := err.Error()
			if publishErr := c.publish(ConnectionEvent{
				Meta:       c.nextHeader(time.Now(), 0, nil),
				State:      Disconnected,
				Reason:     reason,
				Descriptor: c.Descriptor(),
			}); publishErr != nil {
				c.finish(publishErr)
				return
			}
			if !errors.Is(err, io.EOF) && !errors.Is(err, ErrDisconnected) {
				if publishErr := c.publish(ErrorEvent{
					Meta:    c.nextHeader(time.Now(), 0, nil),
					Message: reason,
					Err:     err,
				}); publishErr != nil {
					c.finish(publishErr)
					return
				}
			}
			c.finish(err)
			return
		}

		if observation.ObservedAt.IsZero() {
			observation.ObservedAt = time.Now()
		}
		if observation.Gap != nil {
			if err := c.publish(GapEvent{
				Meta:    c.nextHeader(observation.ObservedAt, observation.DeviceTimestamp, nil),
				Source:  c.descriptor.Backend,
				Dropped: observation.Gap.Dropped,
				Reason:  observation.Gap.Reason,
			}); err != nil {
				c.finish(err)
				return
			}
		}

		c.stateMu.Lock()
		previous := c.state.Clone()
		current := observation.State.Clone()
		c.state = current.Clone()
		c.stateMu.Unlock()

		observationEvent := ObservationEvent{
			Meta:     c.nextHeader(observation.ObservedAt, observation.DeviceTimestamp, nil),
			Native:   observation.Native.clone(),
			Previous: previous,
			Current:  current,
		}
		if err := c.publish(observationEvent); err != nil {
			c.finish(err)
			return
		}
		cause := []EventID{observationEvent.Meta.ID}
		for _, event := range diffEvents(
			previous,
			current,
			func() Header {
				return c.nextHeader(observation.ObservedAt, observation.DeviceTimestamp, cause)
			},
		) {
			if err := c.publish(event); err != nil {
				c.finish(err)
				return
			}
		}
	}
}

func (c *Controller) nextHeader(observedAt time.Time, deviceTimestamp int64, causes []EventID) Header {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.sequence++
	return Header{
		ID: EventID{
			Session:  c.session,
			Stream:   "input",
			Sequence: c.sequence,
		},
		DeviceID:        c.descriptor.ID,
		ObservedAt:      observedAt,
		DeviceTimestamp: deviceTimestamp,
		Causes:          append([]EventID(nil), causes...),
	}
}

func (c *Controller) publish(event Event) error {
	c.pipelineMu.Lock()
	defer c.pipelineMu.Unlock()
	if err := c.dispatch(event); err != nil {
		return err
	}
	return c.processStages([]Event{event}, 0)
}

func (c *Controller) processStages(inputs []Event, start int) error {
	for _, processor := range c.options.processors[start:] {
		var derived []Event
		for _, input := range inputs {
			derived = append(derived, processor.Process(input)...)
		}
		for _, output := range derived {
			if err := c.dispatch(output); err != nil {
				return err
			}
		}
		inputs = append(inputs, derived...)
	}
	return nil
}

func (c *Controller) advanceLoop(done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case now := <-ticker.C:
			for index, processor := range c.options.processors {
				advancing, ok := processor.(AdvancingProcessor)
				if !ok {
					continue
				}
				c.pipelineMu.Lock()
				derived := advancing.Advance(now)
				if len(derived) == 0 {
					c.pipelineMu.Unlock()
					continue
				}
				var err error
				for _, event := range derived {
					if dispatchErr := c.dispatch(event); dispatchErr != nil {
						err = dispatchErr
						break
					}
				}
				if err == nil {
					err = c.processStages(derived, index+1)
				}
				c.pipelineMu.Unlock()
				if err != nil {
					c.setBackgroundError(err)
					c.cancel()
					_ = c.source.Close()
					return
				}
			}
		}
	}
}

func (c *Controller) setBackgroundError(err error) {
	c.backgroundMu.Lock()
	if c.backgroundErr == nil {
		c.backgroundErr = err
	}
	c.backgroundMu.Unlock()
}

func (c *Controller) getBackgroundError() error {
	c.backgroundMu.Lock()
	defer c.backgroundMu.Unlock()
	return c.backgroundErr
}

func (c *Controller) dispatch(event Event) error {
	for _, sink := range c.options.sinks {
		if err := sink.Record(context.WithoutCancel(c.ctx), event); err != nil {
			return fmt.Errorf("record controller event: %w", err)
		}
	}

	c.subsMu.Lock()
	subscribers := make([]*eventSubscription, 0, len(c.subscribers))
	for subscriber := range c.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	c.subsMu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.enqueue(event)
	}
	return nil
}

func (c *Controller) finish(err error) {
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

func (c *Controller) removeSubscription(subscription *eventSubscription) {
	c.subsMu.Lock()
	delete(c.subscribers, subscription)
	c.subsMu.Unlock()
}

func (c *Controller) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.source.Close()
		c.start()
		<-c.done
	})
	return nil
}

type eventSubscription struct {
	controller *Controller
	policy     DeliveryPolicy
	limit      int
	notify     chan struct{}

	mu     sync.Mutex
	queue  []Event
	closed bool
	err    error
}

func (s *eventSubscription) enqueue(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if len(s.queue) >= s.limit {
		if s.policy == DeliveryLatest {
			copy(s.queue, s.queue[1:])
			s.queue[len(s.queue)-1] = event
			s.signal()
			return
		}
		s.closed = true
		s.err = ErrSubscriptionOverflow
		s.queue = nil
		s.signal()
		go s.controller.removeSubscription(s)
		return
	}
	s.queue = append(s.queue, event)
	s.signal()
}

func (s *eventSubscription) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *eventSubscription) Next(ctx context.Context) (Event, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			event := s.queue[0]
			copy(s.queue, s.queue[1:])
			s.queue = s.queue[:len(s.queue)-1]
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
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.notify:
		}
	}
}

func (s *eventSubscription) terminate(err error) {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.err = err
	}
	s.mu.Unlock()
	s.signal()
}

func (s *eventSubscription) Close() error {
	s.terminate(ErrClosed)
	s.controller.removeSubscription(s)
	return nil
}
