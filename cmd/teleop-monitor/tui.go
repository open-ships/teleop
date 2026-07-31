package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/open-ships/teleop"
)

const (
	maxRecentEvents      = 64
	rumbleIntensitySteps = 10
	rumbleIntervalSteps  = 10
	rumbleIntervalStep   = time.Second
)

var (
	accentStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.BrightCyan)
	liveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.BrightGreen)
	recordingStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.BrightRed)
	rumbleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.BrightMagenta)
	mutedStyle = lipgloss.NewStyle().
			Faint(true)
	activeControlStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Black).
				Background(lipgloss.BrightGreen)
	gapStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.BrightWhite).
			Background(lipgloss.Red).
			Padding(0, 1)
	errorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.BrightRed)
)

type controllerEventMsg struct {
	event teleop.Event
	state teleop.State
}

type controllerStreamEndedMsg struct {
	err error
}

type rumbleSetMsg struct {
	enabled  bool
	level    int
	mode     rumbleMode
	side     rumbleSide
	revision uint64
	err      error
}

type rumbleTickMsg struct {
	revision uint64
}

type rumbleMode uint8

const (
	rumbleModeBoth rumbleMode = iota
	rumbleModeAlternate
)

type rumbleSide uint8

const (
	rumbleSideBoth rumbleSide = iota
	rumbleSideLeft
	rumbleSideRight
)

type hapticSlider uint8

const (
	hapticSliderIntensity hapticSlider = iota
	hapticSliderInterval
)

type recentEvent struct {
	number  uint64
	kind    teleop.EventKind
	summary string
}

type monitorModel struct {
	ctx          context.Context
	cancelEvents context.CancelFunc
	controller   teleop.GameController
	subscription teleop.Subscription
	descriptor   teleop.Descriptor
	state        teleop.State
	auditPath    string

	width               int
	height              int
	eventCount          uint64
	observations        uint64
	recent              []recentEvent
	lastGap             string
	streamErr           error
	rumbleOn            bool
	rumblePending       bool
	rumbleTarget        bool
	rumbleTargetMode    rumbleMode
	rumbleTargetSide    rumbleSide
	rumbleLevel         int
	rumbleMode          rumbleMode
	rumbleSide          rumbleSide
	rumbleIntervalLevel int
	hapticSlider        hapticSlider
	rumbleRevision      uint64
	rumbleErr           error
}

func newMonitorModel(
	ctx context.Context,
	cancelEvents context.CancelFunc,
	controller teleop.GameController,
	subscription teleop.Subscription,
	auditPath string,
) *monitorModel {
	return &monitorModel{
		ctx:                 ctx,
		cancelEvents:        cancelEvents,
		controller:          controller,
		subscription:        subscription,
		descriptor:          controller.Descriptor(),
		state:               controller.Snapshot(),
		auditPath:           auditPath,
		width:               96,
		height:              28,
		rumbleLevel:         rumbleIntensitySteps,
		rumbleIntervalLevel: 4,
	}
}

func (m *monitorModel) Init() tea.Cmd {
	return m.waitForEvent()
}

func (m *monitorModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.KeyPressMsg:
		key := message.Keystroke()
		if strings.EqualFold(key, "q") || key == "esc" || key == "ctrl+c" {
			m.cancelEvents()
			return m, tea.Quit
		}
		if strings.EqualFold(key, "r") && !message.Key().IsRepeat {
			return m, m.toggleRumble()
		}
		if strings.EqualFold(key, "m") && !message.Key().IsRepeat {
			return m, m.toggleRumbleMode()
		}
		if key == "tab" {
			m.toggleHapticSlider()
			return m, nil
		}
		switch key {
		case "left":
			return m, m.adjustHapticSlider(-1)
		case "right":
			return m, m.adjustHapticSlider(1)
		}
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
	case controllerEventMsg:
		m.addEvent(message.event, message.state)
		return m, m.waitForEvent()
	case rumbleSetMsg:
		m.rumblePending = false
		if message.err != nil {
			m.rumbleErr = message.err
			return m, nil
		}
		m.rumbleOn = message.enabled
		m.rumbleErr = nil
		m.rumbleSide = message.side
		if !message.enabled {
			m.rumbleSide = rumbleSideBoth
			return m, nil
		}
		if message.revision != m.rumbleRevision ||
			message.mode != m.rumbleMode ||
			message.level != m.rumbleLevel {
			return m, m.requestCurrentRumble()
		}
		if m.rumbleMode == rumbleModeAlternate {
			return m, m.scheduleRumbleTick()
		}
	case rumbleTickMsg:
		if message.revision != m.rumbleRevision ||
			!m.rumbleOn ||
			m.rumblePending ||
			m.rumbleMode != rumbleModeAlternate {
			return m, nil
		}
		side := rumbleSideLeft
		if m.rumbleSide == rumbleSideLeft {
			side = rumbleSideRight
		}
		return m, m.requestRumble(true, side)
	case controllerStreamEndedMsg:
		if !errors.Is(message.err, context.Canceled) &&
			!errors.Is(message.err, teleop.ErrClosed) {
			m.streamErr = message.err
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m *monitorModel) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	view.WindowTitle = "teleop monitor"
	return view
}

func (m *monitorModel) waitForEvent() tea.Cmd {
	return func() tea.Msg {
		event, err := m.subscription.Next(m.ctx)
		if err != nil {
			return controllerStreamEndedMsg{err: err}
		}
		return controllerEventMsg{
			event: event,
			state: m.controller.Snapshot(),
		}
	}
}

func (m *monitorModel) toggleRumble() tea.Cmd {
	if !m.descriptor.Capability.Rumble || m.rumblePending {
		return nil
	}
	if m.rumbleErr != nil {
		if !m.rumbleTarget {
			return m.requestRumble(false, rumbleSideBoth)
		}
		side := m.rumbleTargetSide
		if m.rumbleTargetMode != m.rumbleMode {
			side = m.currentRumbleSide()
		}
		return m.requestRumble(true, side)
	}

	m.rumbleRevision++
	if m.rumbleOn {
		return m.requestRumble(false, rumbleSideBoth)
	}
	return m.requestCurrentRumble()
}

func (m *monitorModel) toggleRumbleMode() tea.Cmd {
	if !m.descriptor.Capability.Rumble {
		return nil
	}
	if m.rumbleMode == rumbleModeBoth {
		m.rumbleMode = rumbleModeAlternate
	} else {
		m.rumbleMode = rumbleModeBoth
		m.hapticSlider = hapticSliderIntensity
	}
	m.rumbleRevision++
	if m.rumblePending || !m.rumbleOn {
		return nil
	}
	return m.requestCurrentRumble()
}

func (m *monitorModel) toggleHapticSlider() {
	if !m.descriptor.Capability.Rumble ||
		m.rumbleMode != rumbleModeAlternate {
		m.hapticSlider = hapticSliderIntensity
		return
	}
	if m.hapticSlider == hapticSliderIntensity {
		m.hapticSlider = hapticSliderInterval
	} else {
		m.hapticSlider = hapticSliderIntensity
	}
}

func (m *monitorModel) adjustHapticSlider(delta int) tea.Cmd {
	if m.hapticSlider == hapticSliderInterval {
		return m.adjustRumbleInterval(delta)
	}
	return m.adjustRumbleIntensity(delta)
}

func (m *monitorModel) adjustRumbleIntensity(delta int) tea.Cmd {
	if !m.descriptor.Capability.Rumble {
		return nil
	}
	level := min(rumbleIntensitySteps, max(0, m.rumbleLevel+delta))
	if level == m.rumbleLevel {
		return nil
	}
	m.rumbleLevel = level
	m.rumbleRevision++
	if m.rumblePending || !m.rumbleOn {
		return nil
	}
	return m.requestCurrentRumble()
}

func (m *monitorModel) adjustRumbleInterval(delta int) tea.Cmd {
	if !m.descriptor.Capability.Rumble ||
		m.rumbleMode != rumbleModeAlternate {
		return nil
	}
	level := min(
		rumbleIntervalSteps-1,
		max(0, m.rumbleIntervalLevel+delta),
	)
	if level == m.rumbleIntervalLevel {
		return nil
	}
	m.rumbleIntervalLevel = level
	m.rumbleRevision++
	if m.rumblePending || !m.rumbleOn {
		return nil
	}
	return m.scheduleRumbleTick()
}

func (m *monitorModel) currentRumbleSide() rumbleSide {
	if m.rumbleMode == rumbleModeBoth {
		return rumbleSideBoth
	}
	if m.rumbleSide == rumbleSideLeft ||
		m.rumbleSide == rumbleSideRight {
		return m.rumbleSide
	}
	return rumbleSideLeft
}

func (m *monitorModel) requestCurrentRumble() tea.Cmd {
	return m.requestRumble(true, m.currentRumbleSide())
}

func (m *monitorModel) requestRumble(
	enabled bool,
	side rumbleSide,
) tea.Cmd {
	if !enabled || m.rumbleMode == rumbleModeBoth {
		side = rumbleSideBoth
	}
	m.rumbleTarget = enabled
	m.rumbleTargetMode = m.rumbleMode
	m.rumbleTargetSide = side
	m.rumblePending = true
	m.rumbleErr = nil
	rumble := teleop.Rumble{}
	if enabled {
		intensity := float32(m.rumbleLevel) / rumbleIntensitySteps
		switch side {
		case rumbleSideLeft:
			rumble.LowFrequency = intensity
		case rumbleSideRight:
			rumble.HighFrequency = intensity
		default:
			rumble.LowFrequency = intensity
			rumble.HighFrequency = intensity
		}
	}
	level := m.rumbleLevel
	mode := m.rumbleMode
	revision := m.rumbleRevision
	return func() tea.Msg {
		return rumbleSetMsg{
			enabled:  enabled,
			level:    level,
			mode:     mode,
			side:     side,
			revision: revision,
			err:      m.controller.SetRumble(m.ctx, rumble),
		}
	}
}

func (m *monitorModel) rumbleInterval() time.Duration {
	return time.Duration(m.rumbleIntervalLevel+1) * rumbleIntervalStep
}

func (m *monitorModel) scheduleRumbleTick() tea.Cmd {
	revision := m.rumbleRevision
	return tea.Tick(m.rumbleInterval(), func(time.Time) tea.Msg {
		return rumbleTickMsg{revision: revision}
	})
}

func (m *monitorModel) addEvent(event teleop.Event, state teleop.State) {
	m.eventCount++
	m.state = state

	if observation, ok := event.(teleop.ObservationEvent); ok {
		m.observations++
		m.state = observation.Current
		m.lastGap = ""
		return
	}
	if observation, ok := event.(*teleop.ObservationEvent); ok {
		m.observations++
		m.state = observation.Current
		m.lastGap = ""
		return
	}

	if gap, ok := event.(teleop.GapEvent); ok {
		m.lastGap = terminalText(gap.Reason)
	}
	if gap, ok := event.(*teleop.GapEvent); ok {
		m.lastGap = terminalText(gap.Reason)
	}

	m.recent = append(m.recent, recentEvent{
		number:  m.eventCount,
		kind:    event.Kind(),
		summary: terminalText(eventSummary(event)),
	})
	if len(m.recent) > maxRecentEvents {
		m.recent = m.recent[len(m.recent)-maxRecentEvents:]
	}
}

func runTUI(
	ctx context.Context,
	controller teleop.GameController,
	subscription teleop.Subscription,
	auditPath string,
) error {
	eventContext, cancelEvents := context.WithCancel(ctx)
	defer cancelEvents()

	model := newMonitorModel(
		eventContext,
		cancelEvents,
		controller,
		subscription,
		auditPath,
	)
	finalModel, err := tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithoutSignalHandler(),
	).Run()
	if err != nil {
		if errors.Is(err, tea.ErrInterrupted) ||
			(errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil) {
			return nil
		}
		return err
	}
	if final, ok := finalModel.(*monitorModel); ok && final.streamErr != nil {
		return final.streamErr
	}
	return nil
}

func (m *monitorModel) render() string {
	width := m.width
	if width <= 0 {
		width = 96
	}
	height := m.height
	if height <= 0 {
		height = 28
	}
	contentWidth := max(30, width-2)

	footer := m.renderFooter(contentWidth)
	parts := []string{
		m.renderHeader(contentWidth),
		m.renderHaptics(contentWidth),
	}
	if m.lastGap != "" {
		recovery := "reconnect the controller"
		if m.auditPath != "" {
			recovery += " and inspect the audit log"
		} else {
			recovery += "; use --audit for future capture"
		}
		alert := gapStyle.Render("INPUT GAP") + " " +
			errorStyle.Render(m.lastGap+" — "+recovery)
		parts = append(parts, alert)
	}
	occupiedHeight := lipgloss.Height(
		lipgloss.JoinVertical(lipgloss.Left, parts...),
	) + lipgloss.Height(footer)

	if contentWidth >= 104 {
		stateWidth := min(58, contentWidth-31)
		eventWidth := contentWidth - stateWidth - 3
		state := m.renderState(stateWidth)
		eventLines := max(5, height-occupiedHeight-2)
		events := m.renderEvents(eventWidth, eventLines)
		separatorHeight := max(lipgloss.Height(state), lipgloss.Height(events))
		separator := mutedStyle.Render(
			strings.TrimSuffix(strings.Repeat(" │ \n", separatorHeight), "\n"),
		)
		parts = append(parts, lipgloss.JoinHorizontal(lipgloss.Top, state, separator, events))
	} else {
		state := m.renderState(contentWidth)
		eventLines := max(
			3,
			height-occupiedHeight-lipgloss.Height(state)-2,
		)
		parts = append(
			parts,
			state,
			m.renderEvents(contentWidth, eventLines),
		)
	}

	parts = append(parts, footer)

	content := lipgloss.JoinVertical(lipgloss.Left, parts...)
	return lipgloss.NewStyle().
		Padding(0, 1).
		MaxWidth(width).
		MaxHeight(height).
		Render(content)
}

func (m *monitorModel) renderHeader(width int) string {
	status := liveStyle.Render("● LIVE")
	if m.auditPath != "" {
		status += "  " + recordingStyle.Render("● REC")
	}
	title := accentStyle.Render("TELEOP MONITOR")

	deviceName := terminalText(m.descriptor.Name)
	if deviceName == "" {
		deviceName = "Game controller"
	}
	metadata := terminalText(fmt.Sprintf(
		"%s  ·  %s  ·  %s  ·  audit %s",
		m.descriptor.Type,
		m.descriptor.Backend,
		m.descriptor.Transport,
		m.descriptor.Capability.AuditGrade,
	))
	if m.auditPath != "" {
		metadata += "  ·  " + terminalText(filepath.Base(m.auditPath))
	}

	return lipgloss.JoinVertical(
		lipgloss.Left,
		padBetween(title, status, width),
		lipgloss.NewStyle().Bold(true).Render(deviceName),
		mutedStyle.MaxWidth(width).Render(metadata),
		mutedStyle.Render(strings.Repeat("─", width)),
	)
}

func (m *monitorModel) renderHaptics(width int) string {
	header := sectionHeader("HAPTIC FEEDBACK", width)
	label := lipgloss.NewStyle().Bold(true).Render("RUMBLE")
	supported := m.descriptor.Capability.Rumble
	var state, action string

	switch {
	case !supported:
		state = mutedStyle.Render("— UNAVAILABLE")
	case m.rumblePending:
		transition := "starting"
		if !m.rumbleTarget {
			transition = "stopping"
		} else if m.rumbleOn {
			transition = "applying"
		}
		state = rumbleStyle.Render("◇ " + strings.ToUpper(transition) + "…")
	case m.rumbleErr != nil:
		if m.rumbleOn {
			state = m.renderRumbleOnState(width)
		} else {
			state = mutedStyle.Render("○ OFF")
		}
	case m.rumbleOn:
		state = m.renderRumbleOnState(width)
		action = keyHint("r", "turn off")
	default:
		state = mutedStyle.Render("○ OFF")
		action = keyHint("r", "turn on")
	}

	status := label + "  " + state
	rows := []string{header}
	if m.rumbleErr != nil {
		failure := errorStyle.Render("ERROR") + " " +
			errorStyle.Render(terminalText(m.rumbleErr.Error()))
		retry := keyHint("r", "retry")
		line := status + "   " + failure + "   " + retry
		if lipgloss.Width(line) <= width {
			rows = append(rows, line)
		} else {
			detail := retry + "   " + failure
			rows = append(
				rows,
				status+"   "+errorStyle.Render("ERROR"),
				lipgloss.NewStyle().MaxWidth(width).Render(detail),
			)
		}
	} else {
		if action != "" {
			status += "   " + action
		}
		mode := m.renderRumbleMode(width)
		if lipgloss.Width(status)+5+lipgloss.Width(mode) <= width {
			rows = append(rows, status+"     "+mode)
		} else {
			rows = append(rows, status, mode)
		}
	}
	if supported {
		if m.rumbleErr != nil {
			rows = append(rows, m.renderRumbleMode(width))
		}
		rows = append(rows, m.renderRumbleIntensity(width))
		if m.rumbleMode == rumbleModeAlternate {
			rows = append(rows, m.renderRumbleInterval(width))
		}
		rows = append(rows, m.renderHapticHelp(width))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m *monitorModel) renderRumbleOnState(width int) string {
	value := "● ON"
	if m.rumbleMode == rumbleModeAlternate {
		side := "LEFT"
		if m.rumbleSide == rumbleSideRight {
			side = "RIGHT"
		}
		if width < 34 {
			side = side[:1]
		}
		value += " · " + side
	}
	return rumbleStyle.Render(value)
}

func (m *monitorModel) renderRumbleMode(width int) string {
	value := "BOTH"
	if m.rumbleMode == rumbleModeAlternate {
		value = "ALTERNATE L/R"
	}
	action := "change"
	if width < 34 {
		if m.rumbleMode == rumbleModeAlternate {
			value = "ALT L/R"
		}
		action = "mode"
	}
	label := lipgloss.NewStyle().Bold(true).Render("MODE")
	return label + "  " +
		lipgloss.NewStyle().Bold(true).Render(value) + "   " +
		keyHint("m", action)
}

func (m *monitorModel) renderRumbleIntensity(width int) string {
	level := min(rumbleIntensitySteps, max(0, m.rumbleLevel))
	value := fmt.Sprintf("%3d%%", level*100/rumbleIntensitySteps)
	return renderHapticSlider(
		"INTENSITY",
		value,
		level,
		rumbleIntensitySteps,
		width,
		m.hapticSlider == hapticSliderIntensity,
	)
}

func (m *monitorModel) renderRumbleInterval(width int) string {
	level := min(
		rumbleIntervalSteps-1,
		max(0, m.rumbleIntervalLevel),
	)
	value := m.rumbleInterval().String()
	return renderHapticSlider(
		"INTERVAL",
		value,
		level,
		rumbleIntervalSteps-1,
		width,
		m.hapticSlider == hapticSliderInterval,
	)
}

func (m *monitorModel) renderHapticHelp(width int) string {
	adjust := keyHint("← / →", "adjust")
	if m.rumbleMode != rumbleModeAlternate {
		return adjust
	}
	selectSlider := keyHint("tab", "select slider")
	if lipgloss.Width(selectSlider)+3+lipgloss.Width(adjust) <= width {
		return selectSlider + "   " + adjust
	}
	return lipgloss.JoinVertical(lipgloss.Left, selectSlider, adjust)
}

func renderHapticSlider(
	label string,
	value string,
	level int,
	maxLevel int,
	width int,
	selected bool,
) string {
	meterWidth := 10
	if width < 34 {
		meterWidth = 7
	}
	filled := 0
	if maxLevel > 0 {
		filled = (level*meterWidth + maxLevel/2) / maxLevel
	}
	meter := rumbleStyle.Render(strings.Repeat("━", filled)) +
		mutedStyle.Render(strings.Repeat("─", meterWidth-filled))
	marker := "  "
	if selected {
		marker = accentStyle.Render("›") + " "
	}
	return marker +
		lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("%-9s", label)) +
		"  [" + meter + "]  " + value
}

func (m *monitorModel) renderFooter(width int) string {
	quit := mutedStyle.Render("q / esc  quit")
	note := mutedStyle.Render("every event remains available with --json")
	if lipgloss.Width(quit)+3+lipgloss.Width(note) <= width {
		return quit + "   " + note
	}
	return quit
}

func (m *monitorModel) renderState(width int) string {
	state := m.state
	leftTrigger := triggerMeter("LT", state.LeftTrigger, meterWidth(width))
	rightTrigger := triggerMeter("RT", state.RightTrigger, meterWidth(width))
	if width >= 54 {
		leftTrigger = padBetween(leftTrigger, rightTrigger, width)
	} else {
		leftTrigger += "\n" + rightTrigger
	}

	shoulders := padBetween(
		"SHOULDERS  "+
			m.control("LB", teleop.ButtonBumperLeft),
		m.control("RB", teleop.ButtonBumperRight),
		width,
	)
	sticks := padBetween(
		fmt.Sprintf(
			"LEFT   x=%+.3f  y=%+.3f  %s",
			state.LeftStick.X,
			state.LeftStick.Y,
			m.control("LS", teleop.ButtonStickLeft),
		),
		fmt.Sprintf(
			"RIGHT  x=%+.3f  y=%+.3f  %s",
			state.RightStick.X,
			state.RightStick.Y,
			m.control("RS", teleop.ButtonStickRight),
		),
		width,
	)
	if width < 72 {
		sticks = fmt.Sprintf(
			"LEFT   x=%+.3f y=%+.3f %s\nRIGHT  x=%+.3f y=%+.3f %s",
			state.LeftStick.X,
			state.LeftStick.Y,
			m.control("LS", teleop.ButtonStickLeft),
			state.RightStick.X,
			state.RightStick.Y,
			m.control("RS", teleop.ButtonStickRight),
		)
	}

	digital := padBetween(
		"D-PAD  "+
			m.control("↑", teleop.DPadUp)+" "+
			m.control("←", teleop.DPadLeft)+" "+
			m.control("↓", teleop.DPadDown)+" "+
			m.control("→", teleop.DPadRight),
		"FACE  "+
			m.control("Y", teleop.ButtonFaceNorth)+" "+
			m.control("X", teleop.ButtonFaceWest)+" "+
			m.control("A", teleop.ButtonFaceSouth)+" "+
			m.control("B", teleop.ButtonFaceEast),
		width,
	)
	if width < 58 {
		digital = "D-PAD  " +
			m.control("↑", teleop.DPadUp) + " " +
			m.control("←", teleop.DPadLeft) + " " +
			m.control("↓", teleop.DPadDown) + " " +
			m.control("→", teleop.DPadRight) + "\n" +
			"FACE   " +
			m.control("Y", teleop.ButtonFaceNorth) + " " +
			m.control("X", teleop.ButtonFaceWest) + " " +
			m.control("A", teleop.ButtonFaceSouth) + " " +
			m.control("B", teleop.ButtonFaceEast)
	}

	system := "SYSTEM  " +
		m.control("View", teleop.ButtonMenuSecondary) + " " +
		m.control("Xbox", teleop.ButtonSystem) + " " +
		m.control("Menu", teleop.ButtonMenuPrimary) + " " +
		m.control("Share", teleop.ButtonCapture)
	paddles := "PADDLES " +
		m.control("P1", teleop.ButtonPaddle1) + " " +
		m.control("P2", teleop.ButtonPaddle2) + " " +
		m.control("P3", teleop.ButtonPaddle3) + " " +
		m.control("P4", teleop.ButtonPaddle4)

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		sectionHeader("INPUT STATE", width),
		shoulders,
		leftTrigger,
		sticks,
		digital,
		system,
		paddles,
	)
	return lipgloss.NewStyle().MaxWidth(width).Render(content)
}

func (m *monitorModel) renderEvents(width, limit int) string {
	counts := fmt.Sprintf(
		"%d received · %d observations",
		m.eventCount,
		m.observations,
	)
	lines := []string{
		sectionHeader("EVENT STREAM", width),
		mutedStyle.MaxWidth(width).Render(counts),
	}
	if len(m.recent) == 0 {
		lines = append(
			lines,
			"",
			mutedStyle.Render("Waiting for controller input…"),
		)
		return lipgloss.JoinVertical(lipgloss.Left, lines...)
	}

	start := max(0, len(m.recent)-limit)
	for _, event := range m.recent[start:] {
		label := eventKindLabel(event.kind)
		prefix := fmt.Sprintf("%06d  %-7s  ", event.number, label)
		lineStyle := lipgloss.NewStyle()
		if event.kind == teleop.EventGap || event.kind == teleop.EventError {
			lineStyle = errorStyle
		}
		line := mutedStyle.Render(prefix) + lineStyle.Render(event.summary)
		lines = append(lines, lipgloss.NewStyle().MaxWidth(width).Render(line))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

func (m *monitorModel) control(label string, id teleop.ControlID) string {
	if len(m.descriptor.Capability.Controls) > 0 &&
		!m.descriptor.Capability.Supports(id) {
		return mutedStyle.Render(" " + label + " ")
	}
	if m.state.Button(id) {
		return activeControlStyle.Render(" " + label + " ")
	}
	return "[" + label + "]"
}

func triggerMeter(label string, value float32, width int) string {
	value = teleop.Clamp(value, 0, 1)
	filled := int(value * float32(width))
	meter := liveStyle.Render(strings.Repeat("━", filled)) +
		mutedStyle.Render(strings.Repeat("─", width-filled))
	return fmt.Sprintf("%s  %s  %5.1f%%", label, meter, value*100)
}

func meterWidth(width int) int {
	if width >= 72 {
		return 12
	}
	if width >= 48 {
		return 10
	}
	return 7
}

func sectionHeader(label string, width int) string {
	title := accentStyle.Render(label)
	remaining := max(1, width-lipgloss.Width(title)-1)
	return title + " " + mutedStyle.Render(strings.Repeat("─", remaining))
}

func keyHint(key, action string) string {
	return accentStyle.Render(key) + mutedStyle.Render("  "+action)
}

func padBetween(left, right string, width int) string {
	spaces := max(width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", spaces) + right
}

func eventKindLabel(kind teleop.EventKind) string {
	switch kind {
	case teleop.EventButton:
		return "button"
	case teleop.EventStick:
		return "stick"
	case teleop.EventTrigger:
		return "trigger"
	case teleop.EventConnection:
		return "device"
	case teleop.EventCapabilities:
		return "caps"
	case teleop.EventGap:
		return "gap"
	case teleop.EventError:
		return "error"
	default:
		value := string(kind)
		if index := strings.LastIndexByte(value, '.'); index >= 0 {
			value = value[index+1:]
		}
		if len(value) > 7 {
			value = value[:7]
		}
		return value
	}
}

func eventSummary(event teleop.Event) string {
	switch value := event.(type) {
	case teleop.ButtonEvent:
		return fmt.Sprintf("%s %s", value.Button, value.Phase)
	case *teleop.ButtonEvent:
		return fmt.Sprintf("%s %s", value.Button, value.Phase)
	case teleop.StickEvent:
		return fmt.Sprintf(
			"%s x=%+.3f y=%+.3f",
			value.Stick,
			value.Position.X,
			value.Position.Y,
		)
	case *teleop.StickEvent:
		return fmt.Sprintf(
			"%s x=%+.3f y=%+.3f",
			value.Stick,
			value.Position.X,
			value.Position.Y,
		)
	case teleop.TriggerEvent:
		return fmt.Sprintf("%s %.3f", value.Trigger, value.Position)
	case *teleop.TriggerEvent:
		return fmt.Sprintf("%s %.3f", value.Trigger, value.Position)
	case teleop.ConnectionEvent:
		return string(value.State)
	case *teleop.ConnectionEvent:
		return string(value.State)
	case teleop.CapabilitiesEvent, *teleop.CapabilitiesEvent:
		return "capabilities updated"
	case teleop.GapEvent:
		return value.Reason
	case *teleop.GapEvent:
		return value.Reason
	case teleop.ErrorEvent:
		return value.Message
	case *teleop.ErrorEvent:
		return value.Message
	default:
		return string(event.Kind())
	}
}
