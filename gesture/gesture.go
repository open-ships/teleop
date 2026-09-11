// Package gesture recognizes temporal and directional patterns in teleop
// input events. It is optional; applications may consume raw input directly.
package gesture

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-ships/teleop"
)

// Type identifies a recognized gesture.
type Type string

const (
	// Tap is a short press followed by release.
	Tap Type = "tap"
	// DoubleTap is two taps within Config.DoubleTapWindow.
	DoubleTap Type = "double-tap"
	// Hold is a press sustained for Config.HoldMinimum.
	Hold Type = "hold"
	// Chord is an exact configured set of simultaneous buttons.
	Chord Type = "chord"
	// StickRegion is entry into or exit from a directional stick region.
	StickRegion Type = "stick-region"
	// TriggerThreshold is a hysteretic trigger threshold crossing.
	TriggerThreshold Type = "trigger-threshold"
)

// EventKind is the persisted kind for gesture events.
const EventKind teleop.EventKind = "gesture"

// Event is a derived temporal or directional input.
type Event struct {
	Meta     teleop.Header      `json:"header"`
	Type     Type               `json:"type"`
	Phase    teleop.Phase       `json:"phase"`
	Controls []teleop.ControlID `json:"controls"`
	Duration time.Duration      `json:"duration,omitempty"`
	Region   string             `json:"region,omitempty"`
	Value    float32            `json:"value,omitempty"`
}

// Header implements teleop.Event.
func (e Event) Header() teleop.Header { return e.Meta.Clone() }

// Kind implements teleop.Event.
func (Event) Kind() teleop.EventKind { return EventKind }

// CloneEvent implements teleop.EventCloner.
func (e Event) CloneEvent() teleop.Event {
	e.Meta = e.Meta.Clone()
	e.Controls = slices.Clone(e.Controls)
	return e
}

// ChordSpec names an exact set of simultaneously pressed buttons.
type ChordSpec struct {
	Name    string
	Buttons []teleop.ControlID
}

// Config controls gesture timing and analog hysteresis.
type Config struct {
	// TapMaximum is the inclusive tap duration limit. Durations between this
	// limit and HoldMinimum produce neither gesture and clear a pending tap.
	// Hold wins at the shared boundary when both limits are equal.
	TapMaximum        time.Duration
	DoubleTapWindow   time.Duration
	HoldMinimum       time.Duration
	StickThreshold    float32
	StickHysteresis   float32
	TriggerThreshold  float32
	TriggerHysteresis float32
	Chords            []ChordSpec
}

// DefaultConfig returns conservative gesture defaults with no duration gap
// between a tap and a hold.
func DefaultConfig() Config {
	return Config{
		TapMaximum:        600 * time.Millisecond,
		DoubleTapWindow:   350 * time.Millisecond,
		HoldMinimum:       600 * time.Millisecond,
		StickThreshold:    0.65,
		StickHysteresis:   0.08,
		TriggerThreshold:  0.5,
		TriggerHysteresis: 0.08,
	}
}

type press struct {
	at       time.Time
	header   teleop.Header
	holdSent bool
	holdID   teleop.EventID
	consumed bool
}

type tap struct {
	at        time.Time
	monotonic time.Duration
	eventID   teleop.EventID
}

type streamKey struct {
	session teleop.SessionID
	device  teleop.DeviceID
}

type streamState struct {
	monotonic    bool
	pressed      map[teleop.ControlID]press
	lastTap      map[teleop.ControlID]tap
	activeChords map[int]teleop.EventID
	stickRegions map[teleop.StickID]string
	triggerDown  map[teleop.TriggerID]bool
}

var recognizerInstances atomic.Uint64

// Recognizer keeps independent state per controller session and is safe for
// concurrent use. Calls within each session must retain event order.
type Recognizer struct {
	mu             sync.Mutex
	config         Config
	states         map[streamKey]*streamState
	fallbackStream string
	fallbackSeq    map[teleop.SessionID]uint64
	processing     teleop.ProcessingContext
}

// New validates config and constructs a recognizer. Invalid configuration
// panics so a contradictory safety policy cannot be silently accepted.
func New(config Config) *Recognizer {
	defaults := DefaultConfig()
	if config.TapMaximum == 0 {
		config.TapMaximum = defaults.TapMaximum
	}
	if config.DoubleTapWindow == 0 {
		config.DoubleTapWindow = defaults.DoubleTapWindow
	}
	if config.HoldMinimum == 0 {
		config.HoldMinimum = defaults.HoldMinimum
	}
	if config.StickThreshold == 0 {
		config.StickThreshold = defaults.StickThreshold
	}
	if config.StickHysteresis == 0 {
		config.StickHysteresis = defaults.StickHysteresis
	}
	if config.TriggerThreshold == 0 {
		config.TriggerThreshold = defaults.TriggerThreshold
	}
	if config.TriggerHysteresis == 0 {
		config.TriggerHysteresis = defaults.TriggerHysteresis
	}
	config.Chords = slices.Clone(config.Chords)
	for index := range config.Chords {
		config.Chords[index].Buttons = canonicalControls(config.Chords[index].Buttons)
	}
	if err := validateConfig(config); err != nil {
		panic(err)
	}
	instance := recognizerInstances.Add(1)
	return &Recognizer{
		config:         config,
		states:         make(map[streamKey]*streamState),
		fallbackStream: fmt.Sprintf("gesture/%d", instance),
		fallbackSeq:    make(map[teleop.SessionID]uint64),
	}
}

func validateConfig(config Config) error {
	for _, value := range []float32{
		config.StickThreshold, config.StickHysteresis,
		config.TriggerThreshold, config.TriggerHysteresis,
	} {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("gesture: thresholds and hysteresis must be finite")
		}
	}
	if config.TapMaximum < 0 ||
		config.DoubleTapWindow < 0 ||
		config.HoldMinimum <= 0 {
		return fmt.Errorf("gesture: durations must be non-negative and hold minimum positive")
	}
	if config.TapMaximum > config.HoldMinimum {
		return fmt.Errorf("gesture: tap maximum %s exceeds hold minimum %s", config.TapMaximum, config.HoldMinimum)
	}
	if config.StickThreshold <= 0 || config.StickThreshold > 1 ||
		config.StickHysteresis < 0 || config.StickHysteresis >= config.StickThreshold {
		return fmt.Errorf("gesture: invalid stick threshold/hysteresis")
	}
	if config.TriggerThreshold <= 0 || config.TriggerThreshold > 1 ||
		config.TriggerHysteresis < 0 || config.TriggerHysteresis >= config.TriggerThreshold {
		return fmt.Errorf("gesture: invalid trigger threshold/hysteresis")
	}
	chordNames := make(map[string]struct{}, len(config.Chords))
	chordControls := make([][]teleop.ControlID, 0, len(config.Chords))
	for _, chord := range config.Chords {
		if chord.Name == "" || len(canonicalControls(chord.Buttons)) < 2 {
			return fmt.Errorf("gesture: chord must have a name and at least two distinct buttons")
		}
		if _, duplicate := chordNames[chord.Name]; duplicate {
			return fmt.Errorf("gesture: duplicate chord name %q", chord.Name)
		}
		chordNames[chord.Name] = struct{}{}
		for _, controls := range chordControls {
			if equalControls(controls, chord.Buttons) {
				return fmt.Errorf("gesture: duplicate chord controls %v", chord.Buttons)
			}
		}
		chordControls = append(chordControls, chord.Buttons)
	}
	return nil
}

// Process implements teleop.Processor.
func (r *Recognizer) Process(input teleop.Event) []teleop.Event {
	recognized := r.Recognize(input)
	return asEvents(recognized)
}

// ProcessContext implements teleop.ContextProcessor.
func (r *Recognizer) ProcessContext(
	_ context.Context,
	processing teleop.ProcessingContext,
	input teleop.Event,
) ([]teleop.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processing = processing
	defer func() { r.processing = nil }()
	recognized := r.recognizeLocked(input)
	return asEvents(recognized), nil
}

// Advance implements teleop.AdvancingProcessor.
func (r *Recognizer) Advance(now time.Time) []teleop.Event {
	recognized := r.AdvanceGestures(now)
	return asEvents(recognized)
}

// AdvanceContext implements teleop.ContextAdvancingProcessor. With a
// MonotonicProcessingContext it advances only that controller's session.
func (r *Recognizer) AdvanceContext(
	_ context.Context,
	processing teleop.ProcessingContext,
	now time.Time,
) ([]teleop.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processing = processing
	defer func() { r.processing = nil }()
	var recognized []Event
	if clock, ok := processing.(teleop.MonotonicProcessingContext); ok {
		session, elapsed := clock.Session(), clock.Monotonic()
		recognized = r.advanceLocked(now, &session, &elapsed)
	} else {
		recognized = r.advanceLocked(now, nil, nil)
	}
	return asEvents(recognized), nil
}

// Recognize consumes one canonical input event. Timing uses session-relative
// monotonic metadata when available, including metadata decoded from audit.
// Legacy events with only wall time retain wall-time timing. Keep one time
// basis per session; after monotonic metadata appears, zero means the session
// origin rather than missing timing. Use AdvanceMonotonic for idle advancement
// of recorded monotonic sessions.
func (r *Recognizer) Recognize(input teleop.Event) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recognizeLocked(input)
}

func (r *Recognizer) recognizeLocked(input teleop.Event) []Event {
	header := input.Header()
	key := streamKey{session: header.ID.Session, device: header.DeviceID}
	state := r.state(key)
	now := state.eventTime(header, r.processing)
	r.expireTaps(state, now)
	switch event := input.(type) {
	case teleop.ButtonEvent:
		return r.processButton(state, event, now)
	case *teleop.ButtonEvent:
		return r.processButton(state, *event, now)
	case teleop.StickEvent:
		return r.processStick(state, event)
	case *teleop.StickEvent:
		return r.processStick(state, *event)
	case teleop.TriggerEvent:
		return r.processTrigger(state, event)
	case *teleop.TriggerEvent:
		return r.processTrigger(state, *event)
	case teleop.ConnectionEvent:
		if event.State == teleop.Disconnected {
			return r.reset(key, state, event.Meta, now)
		}
	case *teleop.ConnectionEvent:
		if event.State == teleop.Disconnected {
			return r.reset(key, state, event.Meta, now)
		}
	}
	return nil
}

// AdvanceGestures emits elapsed holds for legacy wall-time sessions using the
// supplied event-timeline time. Monotonic sessions require AdvanceMonotonic;
// wall time cannot safely advance their timers across a clock correction.
func (r *Recognizer) AdvanceGestures(now time.Time) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.advanceLocked(now, nil, nil)
}

// AdvanceMonotonic emits elapsed holds for one session using its recorded or
// live session-relative monotonic time. It does not consult the host clock or
// advance other sessions. It establishes a monotonic time basis even when the
// only input so far arrived at session origin (zero). Negative values are
// ignored. Use AdvanceGestures for legacy wall-time sessions instead.
func (r *Recognizer) AdvanceMonotonic(session teleop.SessionID, now time.Duration) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.advanceLocked(time.Time{}, &session, &now)
}

func (r *Recognizer) advanceLocked(wall time.Time, session *teleop.SessionID, elapsed *time.Duration) []Event {
	if elapsed != nil && *elapsed < 0 {
		return nil
	}
	keys := slices.SortedFunc(maps.Keys(r.states), func(left, right streamKey) int {
		return cmp.Or(
			bytes.Compare(left.session[:], right.session[:]),
			cmp.Compare(left.device, right.device),
		)
	})
	var result []Event
	for _, key := range keys {
		state := r.states[key]
		if session != nil && key.session != *session {
			continue
		}
		if elapsed != nil {
			state.useMonotonic()
		}
		// Never infer a monotonic duration from wall time or use one
		// controller's session origin to advance another controller.
		if state.monotonic != (elapsed != nil) {
			continue
		}
		now := wall
		if elapsed != nil {
			now = monotonicEpoch.Add(*elapsed)
		}
		r.expireTaps(state, now)
		buttons := sortedPressed(state.pressed)
		for _, button := range buttons {
			pressState := state.pressed[button]
			duration := max(now.Sub(pressState.at), 0)
			if pressState.consumed || pressState.holdSent || duration < r.config.HoldMinimum {
				continue
			}
			pressState.holdSent = true
			startedAt := pressState.header.ObservedAt.Add(r.config.HoldMinimum)
			cause := pressState.header
			if elapsed != nil {
				cause.Monotonic = *elapsed
			}
			event := r.event(
				cause,
				startedAt,
				Hold,
				teleop.PhaseStarted,
				[]teleop.ControlID{button},
				r.config.HoldMinimum,
				"",
				0,
			)
			event.Meta.Causes = []teleop.EventID{pressState.header.ID}
			pressState.holdID = event.Meta.ID
			state.pressed[button] = pressState
			result = append(result, event)
		}
	}
	return result
}

func (r *Recognizer) processButton(state *streamState, event teleop.ButtonEvent, now time.Time) []Event {
	if event.Meta.ObservedAt.IsZero() {
		if r.processing != nil {
			event.Meta.ObservedAt = r.processing.Now()
		} else {
			event.Meta.ObservedAt = time.Now()
		}
		if !state.monotonic {
			now = event.Meta.ObservedAt
		}
	}
	if event.Pressed {
		if _, repeated := state.pressed[event.Button]; repeated {
			return nil
		}
		state.pressed[event.Button] = press{at: now, header: event.Meta}
		return r.startedChords(state, event)
	}

	pressState, found := state.pressed[event.Button]
	delete(state.pressed, event.Button)
	result := r.endedChords(state, event)
	if !found || pressState.consumed {
		return result
	}
	duration := now.Sub(pressState.at)
	if duration < 0 {
		return result
	}
	if pressState.holdSent {
		ended := r.event(
			event.Meta,
			event.Meta.ObservedAt,
			Hold,
			teleop.PhaseEnded,
			[]teleop.ControlID{event.Button},
			duration,
			"",
			0,
		)
		ended.Meta.Causes = []teleop.EventID{pressState.holdID, event.Meta.ID}
		return append(result, ended)
	}
	if duration >= r.config.HoldMinimum {
		// The release reveals an elapsed hold. Preserve the press as its
		// cause, but publication occurs on the release's timeline.
		cause := pressState.header
		cause.Monotonic = event.Meta.Monotonic
		cause.PublishedAt = event.Meta.PublishedAt
		started := r.event(
			cause,
			pressState.header.ObservedAt.Add(r.config.HoldMinimum),
			Hold,
			teleop.PhaseStarted,
			[]teleop.ControlID{event.Button},
			r.config.HoldMinimum,
			"",
			0,
		)
		started.Meta.Causes = []teleop.EventID{pressState.header.ID}
		ended := r.event(
			event.Meta,
			event.Meta.ObservedAt,
			Hold,
			teleop.PhaseEnded,
			[]teleop.ControlID{event.Button},
			duration,
			"",
			0,
		)
		ended.Meta.Causes = []teleop.EventID{started.Meta.ID, event.Meta.ID}
		return append(result, started, ended)
	}

	if duration > r.config.TapMaximum {
		delete(state.lastTap, event.Button)
		return result
	}
	if last, ok := state.lastTap[event.Button]; ok && !now.Before(last.at) {
		since := now.Sub(last.at)
		if since <= r.config.DoubleTapWindow {
			doubleTapped := r.event(
				event.Meta,
				event.Meta.ObservedAt,
				DoubleTap,
				teleop.PhaseEnded,
				[]teleop.ControlID{event.Button},
				since,
				"",
				0,
			)
			doubleTapped.Meta.Causes = []teleop.EventID{
				last.eventID,
				pressState.header.ID,
				event.Meta.ID,
			}
			delete(state.lastTap, event.Button)
			return append(result, doubleTapped)
		}
	}
	tapped := r.event(
		event.Meta,
		event.Meta.ObservedAt,
		Tap,
		teleop.PhaseEnded,
		[]teleop.ControlID{event.Button},
		duration,
		"",
		0,
	)
	tapped.Meta.Causes = []teleop.EventID{pressState.header.ID, event.Meta.ID}
	state.lastTap[event.Button] = tap{at: now, monotonic: headerMonotonic(event.Meta), eventID: tapped.Meta.ID}
	return append(result, tapped)
}

func (r *Recognizer) startedChords(
	state *streamState,
	event teleop.ButtonEvent,
) []Event {
	var result []Event
	for index, chord := range r.config.Chords {
		if _, active := state.activeChords[index]; active ||
			!contains(chord.Buttons, event.Button) {
			continue
		}
		allPressed := true
		for _, button := range chord.Buttons {
			if _, pressed := state.pressed[button]; !pressed {
				allPressed = false
				break
			}
		}
		if !allPressed {
			continue
		}
		started := r.event(
			event.Meta,
			event.Meta.ObservedAt,
			Chord,
			teleop.PhaseStarted,
			slices.Clone(chord.Buttons),
			0,
			chord.Name,
			0,
		)
		started.Meta.Causes = appendChordCauses(nil, state.pressed, chord.Buttons)
		state.activeChords[index] = started.Meta.ID
		for _, button := range chord.Buttons {
			value := state.pressed[button]
			value.consumed = true
			state.pressed[button] = value
			delete(state.lastTap, button)
		}
		result = append(result, started)
	}
	return result
}

func (r *Recognizer) endedChords(
	state *streamState,
	event teleop.ButtonEvent,
) []Event {
	var result []Event
	for index, chord := range r.config.Chords {
		startedID, active := state.activeChords[index]
		if !active || !contains(chord.Buttons, event.Button) {
			continue
		}
		delete(state.activeChords, index)
		ended := r.event(
			event.Meta,
			event.Meta.ObservedAt,
			Chord,
			teleop.PhaseEnded,
			slices.Clone(chord.Buttons),
			0,
			chord.Name,
			0,
		)
		ended.Meta.Causes = []teleop.EventID{startedID, event.Meta.ID}
		result = append(result, ended)
	}
	return result
}

func (r *Recognizer) processStick(state *streamState, event teleop.StickEvent) []Event {
	control, ok := stickControl(event.Stick)
	if !ok {
		return nil
	}
	previous := state.stickRegions[event.Stick]
	threshold := r.config.StickThreshold
	if previous != "" {
		threshold -= r.config.StickHysteresis
	}
	region := stickRegion(event.Position, threshold)
	if region == previous {
		return nil
	}
	state.stickRegions[event.Stick] = region
	var result []Event
	if previous != "" {
		result = append(result, r.event(
			event.Meta, event.Meta.ObservedAt, StickRegion, teleop.PhaseEnded,
			[]teleop.ControlID{control}, 0, previous, 0,
		))
	}
	if region != "" {
		result = append(result, r.event(
			event.Meta, event.Meta.ObservedAt, StickRegion, teleop.PhaseStarted,
			[]teleop.ControlID{control}, 0, region, 0,
		))
	}
	return result
}

func (r *Recognizer) processTrigger(state *streamState, event teleop.TriggerEvent) []Event {
	wasDown := state.triggerDown[event.Trigger]
	threshold := r.config.TriggerThreshold
	if wasDown {
		threshold -= r.config.TriggerHysteresis
	}
	down := event.Position >= threshold
	if wasDown == down {
		return nil
	}
	control, ok := triggerControl(event.Trigger)
	if !ok {
		return nil
	}
	state.triggerDown[event.Trigger] = down
	phase := teleop.PhaseEnded
	if down {
		phase = teleop.PhaseStarted
	}
	return []Event{r.event(
		event.Meta, event.Meta.ObservedAt, TriggerThreshold, phase,
		[]teleop.ControlID{control}, 0, "", event.Position,
	)}
}

func (r *Recognizer) reset(
	key streamKey,
	state *streamState,
	cause teleop.Header,
	now time.Time,
) []Event {
	var result []Event
	for index, chord := range r.config.Chords {
		startedID, active := state.activeChords[index]
		if !active {
			continue
		}
		ended := r.event(
			cause, cause.ObservedAt, Chord, teleop.PhaseEnded,
			slices.Clone(chord.Buttons), 0, chord.Name, 0,
		)
		ended.Meta.Causes = []teleop.EventID{startedID, cause.ID}
		result = append(result, ended)
	}
	for _, button := range sortedPressed(state.pressed) {
		value := state.pressed[button]
		if value.consumed || !value.holdSent {
			continue
		}
		duration := max(now.Sub(value.at), 0)
		ended := r.event(
			cause, cause.ObservedAt, Hold, teleop.PhaseEnded,
			[]teleop.ControlID{button}, duration, "", 0,
		)
		ended.Meta.Causes = []teleop.EventID{value.holdID, cause.ID}
		result = append(result, ended)
	}
	sticks := make([]string, 0, len(state.stickRegions))
	for stick, region := range state.stickRegions {
		if region != "" {
			sticks = append(sticks, string(stick))
		}
	}
	slices.Sort(sticks)
	for _, raw := range sticks {
		stick := teleop.StickID(raw)
		control, ok := stickControl(stick)
		if ok {
			result = append(result, r.event(
				cause, cause.ObservedAt, StickRegion, teleop.PhaseEnded,
				[]teleop.ControlID{control}, 0, state.stickRegions[stick], 0,
			))
		}
	}
	triggers := make([]string, 0, len(state.triggerDown))
	for trigger, down := range state.triggerDown {
		if down {
			triggers = append(triggers, string(trigger))
		}
	}
	slices.Sort(triggers)
	for _, raw := range triggers {
		trigger := teleop.TriggerID(raw)
		control, ok := triggerControl(trigger)
		if ok {
			result = append(result, r.event(
				cause, cause.ObservedAt, TriggerThreshold, teleop.PhaseEnded,
				[]teleop.ControlID{control}, 0, "", 0,
			))
		}
	}
	delete(r.states, key)
	return result
}

func (r *Recognizer) event(
	cause teleop.Header,
	observedAt time.Time,
	kind Type,
	phase teleop.Phase,
	controls []teleop.ControlID,
	duration time.Duration,
	region string,
	value float32,
) Event {
	var header teleop.Header
	if r.processing != nil {
		header = r.processing.NewHeader(
			"gesture",
			observedAt,
			cause.DeviceTimestamp,
			cause.ID,
		)
		header.Synthetic = cause.Synthetic
	} else {
		r.fallbackSeq[cause.ID.Session]++
		header = teleop.Header{
			ID: teleop.EventID{
				Session:  cause.ID.Session,
				Stream:   r.fallbackStream,
				Sequence: r.fallbackSeq[cause.ID.Session],
			},
			DeviceID:          cause.DeviceID,
			ObservedAt:        observedAt,
			ReceivedAt:        cause.ReceivedAt,
			PublishedAt:       cause.PublishedAt,
			Monotonic:         cause.Monotonic,
			ReceivedMonotonic: cause.ReceivedMonotonic,
			DeviceTimestamp:   cause.DeviceTimestamp,
			Causes:            []teleop.EventID{cause.ID},
			Synthetic:         cause.Synthetic,
		}
	}
	return Event{
		Meta:     header,
		Type:     kind,
		Phase:    phase,
		Controls: slices.Clone(controls),
		Duration: duration,
		Region:   region,
		Value:    value,
	}
}

func (r *Recognizer) state(key streamKey) *streamState {
	state := r.states[key]
	if state == nil {
		state = &streamState{
			pressed:      make(map[teleop.ControlID]press),
			lastTap:      make(map[teleop.ControlID]tap),
			activeChords: make(map[int]teleop.EventID),
			stickRegions: make(map[teleop.StickID]string),
			triggerDown:  make(map[teleop.TriggerID]bool),
		}
		r.states[key] = state
	}
	return state
}

func (r *Recognizer) expireTaps(state *streamState, now time.Time) {
	if now.IsZero() {
		return
	}
	for control, last := range state.lastTap {
		if now.Sub(last.at) > r.config.DoubleTapWindow {
			delete(state.lastTap, control)
		}
	}
}

// asEvents widens recognized gestures to the open teleop.Event interface.
func asEvents[T teleop.Event](values []T) []teleop.Event {
	result := make([]teleop.Event, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

func sortedPressed(values map[teleop.ControlID]press) []teleop.ControlID {
	return slices.Sorted(maps.Keys(values))
}

func canonicalControls(values []teleop.ControlID) []teleop.ControlID {
	sorted := slices.Compact(slices.Sorted(slices.Values(values)))
	return slices.DeleteFunc(sorted, func(value teleop.ControlID) bool { return value == "" })
}

func equalControls(left, right []teleop.ControlID) bool {
	return slices.Equal(left, right)
}

// contains reports membership in a canonical (sorted, deduplicated) control list.
func contains(values []teleop.ControlID, value teleop.ControlID) bool {
	_, found := slices.BinarySearch(values, value)
	return found
}

func appendChordCauses(
	causes []teleop.EventID,
	pressed map[teleop.ControlID]press,
	buttons []teleop.ControlID,
) []teleop.EventID {
	seen := make(map[teleop.EventID]bool, len(causes))
	for _, cause := range causes {
		seen[cause] = true
	}
	for _, button := range buttons {
		if state, ok := pressed[button]; ok && !seen[state.header.ID] {
			causes = append(causes, state.header.ID)
			seen[state.header.ID] = true
		}
	}
	return causes
}

func stickControl(stick teleop.StickID) (teleop.ControlID, bool) {
	switch stick {
	case teleop.LeftStick:
		return teleop.StickLeft, true
	case teleop.RightStick:
		return teleop.StickRight, true
	default:
		return "", false
	}
}

func triggerControl(trigger teleop.TriggerID) (teleop.ControlID, bool) {
	switch trigger {
	case teleop.LeftTrigger:
		return teleop.TriggerLeft, true
	case teleop.RightTrigger:
		return teleop.TriggerRight, true
	default:
		return "", false
	}
}

func stickRegion(stick teleop.Stick, threshold float32) string {
	magnitude := math.Hypot(float64(stick.X), float64(stick.Y))
	if magnitude < float64(threshold) {
		return ""
	}
	angle := math.Atan2(float64(stick.Y), float64(stick.X)) * 180 / math.Pi
	switch {
	case angle >= -22.5 && angle < 22.5:
		return "east"
	case angle >= 22.5 && angle < 67.5:
		return "north-east"
	case angle >= 67.5 && angle < 112.5:
		return "north"
	case angle >= 112.5 && angle < 157.5:
		return "north-west"
	case angle >= 157.5 || angle < -157.5:
		return "west"
	case angle >= -157.5 && angle < -112.5:
		return "south-west"
	case angle >= -112.5 && angle < -67.5:
		return "south"
	default:
		return "south-east"
	}
}
