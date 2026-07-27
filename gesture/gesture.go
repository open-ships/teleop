// Package gesture recognizes temporal and directional patterns in teleop
// input events. It is optional; applications may consume raw input directly.
package gesture

import (
	"math"
	"sync"
	"time"

	"github.com/open-ships/teleop"
)

type Type string

const (
	Tap              Type = "tap"
	DoubleTap        Type = "double-tap"
	Hold             Type = "hold"
	Chord            Type = "chord"
	StickRegion      Type = "stick-region"
	TriggerThreshold Type = "trigger-threshold"
)

const EventKind teleop.EventKind = "gesture"

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

type ChordSpec struct {
	Name    string
	Buttons []teleop.ControlID
}

type Config struct {
	TapMaximum       time.Duration
	DoubleTapWindow  time.Duration
	HoldMinimum      time.Duration
	StickThreshold   float32
	TriggerThreshold float32
	Chords           []ChordSpec
}

func DefaultConfig() Config {
	return Config{
		TapMaximum:       250 * time.Millisecond,
		DoubleTapWindow:  350 * time.Millisecond,
		HoldMinimum:      600 * time.Millisecond,
		StickThreshold:   0.65,
		TriggerThreshold: 0.5,
	}
}

type press struct {
	at       time.Time
	header   teleop.Header
	holdSent bool
}

type tap struct {
	at      time.Time
	eventID teleop.EventID
}

// Recognizer is safe for concurrent use, though ordered calls from one event
// stream are recommended.
type Recognizer struct {
	mu     sync.Mutex
	config Config
	stream uint64

	pressed      map[teleop.ControlID]press
	lastTap      map[teleop.ControlID]tap
	activeChords map[int]bool
	stickRegions map[teleop.StickID]string
	triggerDown  map[teleop.TriggerID]bool
}

func New(config Config) *Recognizer {
	defaults := DefaultConfig()
	if config.TapMaximum <= 0 {
		config.TapMaximum = defaults.TapMaximum
	}
	if config.DoubleTapWindow <= 0 {
		config.DoubleTapWindow = defaults.DoubleTapWindow
	}
	if config.HoldMinimum <= 0 {
		config.HoldMinimum = defaults.HoldMinimum
	}
	if config.StickThreshold <= 0 {
		config.StickThreshold = defaults.StickThreshold
	}
	if config.TriggerThreshold <= 0 {
		config.TriggerThreshold = defaults.TriggerThreshold
	}
	return &Recognizer{
		config:       config,
		pressed:      make(map[teleop.ControlID]press),
		lastTap:      make(map[teleop.ControlID]tap),
		activeChords: make(map[int]bool),
		stickRegions: make(map[teleop.StickID]string),
		triggerDown:  make(map[teleop.TriggerID]bool),
	}
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

// Advance implements teleop.AdvancingProcessor.
func (r *Recognizer) Advance(now time.Time) []teleop.Event {
	recognized := r.AdvanceGestures(now)
	result := make([]teleop.Event, len(recognized))
	for index := range recognized {
		result[index] = recognized[index]
	}
	return result
}

// Recognize accepts one canonical input event and returns zero or more
// strongly typed gestures. Returned events cite the input event that caused
// them.
func (r *Recognizer) Recognize(input teleop.Event) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch event := input.(type) {
	case teleop.ButtonEvent:
		return r.processButton(event)
	case *teleop.ButtonEvent:
		return r.processButton(*event)
	case teleop.StickEvent:
		return r.processStick(event)
	case *teleop.StickEvent:
		return r.processStick(*event)
	case teleop.TriggerEvent:
		return r.processTrigger(event)
	case *teleop.TriggerEvent:
		return r.processTrigger(*event)
	default:
		return nil
	}
}

// AdvanceGestures emits strongly typed holds whose duration has elapsed even
// when no new input event arrives.
func (r *Recognizer) AdvanceGestures(now time.Time) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	var result []Event
	for button, state := range r.pressed {
		if state.holdSent || now.Sub(state.at) < r.config.HoldMinimum {
			continue
		}
		state.holdSent = true
		r.pressed[button] = state
		header := teleop.Header{
			ID:         r.nextID(state.header.ID.Session),
			DeviceID:   state.header.DeviceID,
			ObservedAt: now,
			Causes:     []teleop.EventID{state.header.ID},
		}
		result = append(result, Event{
			Meta:     header,
			Type:     Hold,
			Phase:    teleop.PhaseStarted,
			Controls: []teleop.ControlID{button},
			Duration: now.Sub(state.at),
		})
	}
	return result
}

func (r *Recognizer) processButton(event teleop.ButtonEvent) []Event {
	now := event.Meta.ObservedAt
	if now.IsZero() {
		now = time.Now()
	}
	if event.Pressed {
		r.pressed[event.Button] = press{at: now, header: event.Meta}
		return r.startedChords(event)
	}

	state, found := r.pressed[event.Button]
	delete(r.pressed, event.Button)
	var result []Event
	for index, active := range r.activeChords {
		chord := r.config.Chords[index]
		if !active || !contains(chord.Buttons, event.Button) {
			continue
		}
		r.activeChords[index] = false
		ended := r.event(
			event.Meta,
			Chord,
			teleop.PhaseEnded,
			append([]teleop.ControlID(nil), chord.Buttons...),
			0,
			chord.Name,
			0,
		)
		ended.Meta.Causes = appendChordCauses(ended.Meta.Causes, r.pressed, chord.Buttons)
		result = append(result, ended)
	}
	if !found {
		return result
	}

	duration := now.Sub(state.at)
	if state.holdSent {
		ended := r.event(
			event.Meta,
			Hold,
			teleop.PhaseEnded,
			[]teleop.ControlID{event.Button},
			duration,
			"",
			0,
		)
		ended.Meta.Causes = []teleop.EventID{state.header.ID, event.Meta.ID}
		result = append(result, ended)
		return result
	}
	if duration >= r.config.HoldMinimum {
		started := r.event(
			event.Meta,
			Hold,
			teleop.PhaseStarted,
			[]teleop.ControlID{event.Button},
			duration,
			"",
			0,
		)
		started.Meta.Causes = []teleop.EventID{state.header.ID, event.Meta.ID}
		ended := r.event(
			event.Meta,
			Hold,
			teleop.PhaseEnded,
			[]teleop.ControlID{event.Button},
			duration,
			"",
			0,
		)
		ended.Meta.Causes = []teleop.EventID{started.Meta.ID}
		result = append(result, started, ended)
		return result
	}
	if duration > r.config.TapMaximum {
		return result
	}
	tapped := r.event(
		event.Meta,
		Tap,
		teleop.PhaseEnded,
		[]teleop.ControlID{event.Button},
		duration,
		"",
		0,
	)
	tapped.Meta.Causes = []teleop.EventID{state.header.ID, event.Meta.ID}
	result = append(result, tapped)
	if last := r.lastTap[event.Button]; !last.at.IsZero() && now.Sub(last.at) <= r.config.DoubleTapWindow {
		doubleTapped := r.event(
			event.Meta,
			DoubleTap,
			teleop.PhaseEnded,
			[]teleop.ControlID{event.Button},
			now.Sub(last.at),
			"",
			0,
		)
		doubleTapped.Meta.Causes = []teleop.EventID{last.eventID, tapped.Meta.ID}
		result = append(result, doubleTapped)
		delete(r.lastTap, event.Button)
	} else {
		r.lastTap[event.Button] = tap{at: now, eventID: tapped.Meta.ID}
	}
	return result
}

func (r *Recognizer) startedChords(event teleop.ButtonEvent) []Event {
	var result []Event
	for index, chord := range r.config.Chords {
		if r.activeChords[index] || !contains(chord.Buttons, event.Button) {
			continue
		}
		allPressed := true
		for _, button := range chord.Buttons {
			if _, pressed := r.pressed[button]; !pressed {
				allPressed = false
				break
			}
		}
		if allPressed {
			r.activeChords[index] = true
			started := r.event(
				event.Meta,
				Chord,
				teleop.PhaseStarted,
				append([]teleop.ControlID(nil), chord.Buttons...),
				0,
				chord.Name,
				0,
			)
			started.Meta.Causes = appendChordCauses(nil, r.pressed, chord.Buttons)
			result = append(result, started)
		}
	}
	return result
}

func (r *Recognizer) processStick(event teleop.StickEvent) []Event {
	region := stickRegion(event.Position, r.config.StickThreshold)
	previous := r.stickRegions[event.Stick]
	if region == previous {
		return nil
	}
	r.stickRegions[event.Stick] = region
	control := teleop.StickRight
	if event.Stick == teleop.LeftStick {
		control = teleop.StickLeft
	}
	var result []Event
	if previous != "" {
		result = append(result, r.event(
			event.Meta,
			StickRegion,
			teleop.PhaseEnded,
			[]teleop.ControlID{control},
			0,
			previous,
			0,
		))
	}
	if region != "" {
		result = append(result, r.event(
			event.Meta,
			StickRegion,
			teleop.PhaseStarted,
			[]teleop.ControlID{control},
			0,
			region,
			0,
		))
	}
	return result
}

func (r *Recognizer) processTrigger(event teleop.TriggerEvent) []Event {
	down := event.Position >= r.config.TriggerThreshold
	if r.triggerDown[event.Trigger] == down {
		return nil
	}
	r.triggerDown[event.Trigger] = down
	control := teleop.TriggerRight
	if event.Trigger == teleop.LeftTrigger {
		control = teleop.TriggerLeft
	}
	phase := teleop.PhaseEnded
	if down {
		phase = teleop.PhaseStarted
	}
	return []Event{r.event(
		event.Meta,
		TriggerThreshold,
		phase,
		[]teleop.ControlID{control},
		0,
		"",
		event.Position,
	)}
}

func (r *Recognizer) event(
	cause teleop.Header,
	kind Type,
	phase teleop.Phase,
	controls []teleop.ControlID,
	duration time.Duration,
	region string,
	value float32,
) Event {
	return Event{
		Meta: teleop.Header{
			ID:         r.nextID(cause.ID.Session),
			DeviceID:   cause.DeviceID,
			ObservedAt: cause.ObservedAt,
			Causes:     []teleop.EventID{cause.ID},
		},
		Type:     kind,
		Phase:    phase,
		Controls: controls,
		Duration: duration,
		Region:   region,
		Value:    value,
	}
}

func (r *Recognizer) nextID(session teleop.SessionID) teleop.EventID {
	r.stream++
	return teleop.EventID{
		Session:  session,
		Stream:   "gesture",
		Sequence: r.stream,
	}
}

func contains(values []teleop.ControlID, value teleop.ControlID) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
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
