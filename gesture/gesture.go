// Package gesture recognizes temporal and directional patterns in teleop
// input events. It is optional; applications may consume raw input directly.
package gesture

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-ships/teleop"
)

// Type identifies a recognized gesture.
type Type string

const (
	Tap              Type = "tap"
	DoubleTap        Type = "double-tap"
	Hold             Type = "hold"
	Chord            Type = "chord"
	StickRegion      Type = "stick-region"
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

func (e Event) Header() teleop.Header { return e.Meta.Clone() }
func (Event) Kind() teleop.EventKind  { return EventKind }
func (e Event) CloneEvent() teleop.Event {
	e.Meta = e.Meta.Clone()
	e.Controls = append([]teleop.ControlID(nil), e.Controls...)
	return e
}

// ChordSpec names an exact set of simultaneously pressed buttons.
type ChordSpec struct {
	Name    string
	Buttons []teleop.ControlID
}

// Config controls gesture timing and analog hysteresis.
type Config struct {
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
	at      time.Time
	eventID teleop.EventID
}

type streamKey struct {
	session teleop.SessionID
	device  teleop.DeviceID
}

type streamState struct {
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
	config.Chords = append([]ChordSpec(nil), config.Chords...)
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
	result := make([]teleop.Event, len(recognized))
	for index := range recognized {
		result[index] = recognized[index]
	}
	return result
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
	result := make([]teleop.Event, len(recognized))
	for index := range recognized {
		result[index] = recognized[index]
	}
	return result, nil
}

// Advance implements teleop.AdvancingProcessor.
func (r *Recognizer) Advance(now time.Time) []teleop.Event {
	recognized := r.AdvanceGestures(now)
	result := make([]teleop.Event, len(recognized))
	for index := range recognized {
		result[index] = recognized[index]
	}
	return result
}

// AdvanceContext implements teleop.ContextAdvancingProcessor.
func (r *Recognizer) AdvanceContext(
	_ context.Context,
	processing teleop.ProcessingContext,
	now time.Time,
) ([]teleop.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processing = processing
	defer func() { r.processing = nil }()
	recognized := r.advanceLocked(now)
	result := make([]teleop.Event, len(recognized))
	for index := range recognized {
		result[index] = recognized[index]
	}
	return result, nil
}

// Recognize consumes one canonical input event.
func (r *Recognizer) Recognize(input teleop.Event) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recognizeLocked(input)
}

func (r *Recognizer) recognizeLocked(input teleop.Event) []Event {
	header := input.Header()
	key := streamKey{session: header.ID.Session, device: header.DeviceID}
	state := r.state(key)
	r.expireTaps(state, header.ObservedAt)
	switch event := input.(type) {
	case teleop.ButtonEvent:
		return r.processButton(state, event)
	case *teleop.ButtonEvent:
		return r.processButton(state, *event)
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
			return r.reset(key, state, event.Meta)
		}
	case *teleop.ConnectionEvent:
		if event.State == teleop.Disconnected {
			return r.reset(key, state, event.Meta)
		}
	}
	return nil
}

// AdvanceGestures emits elapsed holds using the supplied event-timeline time.
func (r *Recognizer) AdvanceGestures(now time.Time) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.advanceLocked(now)
}

func (r *Recognizer) advanceLocked(now time.Time) []Event {
	keys := make([]streamKey, 0, len(r.states))
	for key := range r.states {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left := keys[i].session.String() + string(keys[i].device)
		right := keys[j].session.String() + string(keys[j].device)
		return left < right
	})
	var result []Event
	for _, key := range keys {
		state := r.states[key]
		r.expireTaps(state, now)
		buttons := sortedPressed(state.pressed)
		for _, button := range buttons {
			pressState := state.pressed[button]
			duration := nonNegative(now.Sub(pressState.at))
			if pressState.consumed || pressState.holdSent || duration < r.config.HoldMinimum {
				continue
			}
			pressState.holdSent = true
			startedAt := pressState.at.Add(r.config.HoldMinimum)
			event := r.event(
				pressState.header,
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

func (r *Recognizer) processButton(state *streamState, event teleop.ButtonEvent) []Event {
	now := event.Meta.ObservedAt
	if now.IsZero() {
		if r.processing != nil {
			now = r.processing.Now()
		} else {
			now = time.Now()
		}
		event.Meta.ObservedAt = now
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
			now,
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
		started := r.event(
			pressState.header,
			pressState.at.Add(r.config.HoldMinimum),
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
			now,
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

	if last, ok := state.lastTap[event.Button]; ok && !now.Before(last.at) {
		since := now.Sub(last.at)
		if since <= r.config.DoubleTapWindow {
			doubleTapped := r.event(
				event.Meta,
				now,
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
		now,
		Tap,
		teleop.PhaseEnded,
		[]teleop.ControlID{event.Button},
		duration,
		"",
		0,
	)
	tapped.Meta.Causes = []teleop.EventID{pressState.header.ID, event.Meta.ID}
	state.lastTap[event.Button] = tap{at: now, eventID: tapped.Meta.ID}
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
			append([]teleop.ControlID(nil), chord.Buttons...),
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
			append([]teleop.ControlID(nil), chord.Buttons...),
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
) []Event {
	var result []Event
	for index, chord := range r.config.Chords {
		startedID, active := state.activeChords[index]
		if !active {
			continue
		}
		ended := r.event(
			cause, cause.ObservedAt, Chord, teleop.PhaseEnded,
			append([]teleop.ControlID(nil), chord.Buttons...), 0, chord.Name, 0,
		)
		ended.Meta.Causes = []teleop.EventID{startedID, cause.ID}
		result = append(result, ended)
	}
	for _, button := range sortedPressed(state.pressed) {
		value := state.pressed[button]
		if value.consumed || !value.holdSent {
			continue
		}
		duration := nonNegative(cause.ObservedAt.Sub(value.at))
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
	sort.Strings(sticks)
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
	sort.Strings(triggers)
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
			DeviceID:        cause.DeviceID,
			ObservedAt:      observedAt,
			ReceivedAt:      cause.ReceivedAt,
			PublishedAt:     cause.PublishedAt,
			DeviceTimestamp: cause.DeviceTimestamp,
			Causes:          []teleop.EventID{cause.ID},
			Synthetic:       cause.Synthetic,
		}
	}
	return Event{
		Meta:     header,
		Type:     kind,
		Phase:    phase,
		Controls: append([]teleop.ControlID(nil), controls...),
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

func sortedPressed(values map[teleop.ControlID]press) []teleop.ControlID {
	result := make([]teleop.ControlID, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func canonicalControls(values []teleop.ControlID) []teleop.ControlID {
	seen := make(map[teleop.ControlID]struct{}, len(values))
	result := make([]teleop.ControlID, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func equalControls(left, right []teleop.ControlID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func contains(values []teleop.ControlID, value teleop.ControlID) bool {
	index := sort.Search(len(values), func(index int) bool { return values[index] >= value })
	return index < len(values) && values[index] == value
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

func nonNegative(duration time.Duration) time.Duration {
	if duration < 0 {
		return 0
	}
	return duration
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
