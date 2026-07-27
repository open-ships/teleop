package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/open-ships/teleop"
)

const maxRecentEvents = 64

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

	width        int
	height       int
	eventCount   uint64
	observations uint64
	recent       []recentEvent
	lastGap      string
	streamErr    error
}

func newMonitorModel(
	ctx context.Context,
	cancelEvents context.CancelFunc,
	controller teleop.GameController,
	subscription teleop.Subscription,
	auditPath string,
) *monitorModel {
	return &monitorModel{
		ctx:          ctx,
		cancelEvents: cancelEvents,
		controller:   controller,
		subscription: subscription,
		descriptor:   controller.Descriptor(),
		state:        controller.Snapshot(),
		auditPath:    auditPath,
		width:        96,
		height:       28,
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
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
	case controllerEventMsg:
		m.addEvent(message.event, message.state)
		return m, m.waitForEvent()
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

func (m *monitorModel) addEvent(event teleop.Event, state teleop.State) {
	m.eventCount++
	m.state = state

	if observation, ok := event.(teleop.ObservationEvent); ok {
		m.observations++
		m.state = observation.Current
		return
	}
	if observation, ok := event.(*teleop.ObservationEvent); ok {
		m.observations++
		m.state = observation.Current
		return
	}

	if gap, ok := event.(teleop.GapEvent); ok {
		m.lastGap = gap.Reason
	}
	if gap, ok := event.(*teleop.GapEvent); ok {
		m.lastGap = gap.Reason
	}

	m.recent = append(m.recent, recentEvent{
		number:  m.eventCount,
		kind:    event.Kind(),
		summary: eventSummary(event),
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

	parts := []string{m.renderHeader(contentWidth)}
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

	if contentWidth >= 104 {
		stateWidth := min(58, contentWidth-31)
		eventWidth := contentWidth - stateWidth - 3
		state := m.renderState(stateWidth)
		events := m.renderEvents(eventWidth, max(5, height-7))
		separatorHeight := max(lipgloss.Height(state), lipgloss.Height(events))
		separator := mutedStyle.Render(
			strings.TrimSuffix(strings.Repeat(" │ \n", separatorHeight), "\n"),
		)
		parts = append(parts, lipgloss.JoinHorizontal(lipgloss.Top, state, separator, events))
	} else {
		eventLines := max(3, height-18)
		parts = append(
			parts,
			m.renderState(contentWidth),
			m.renderEvents(contentWidth, eventLines),
		)
	}

	footer := mutedStyle.Render("q / esc  quit") + "   " +
		mutedStyle.Render("every event remains available with --json")
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

	deviceName := m.descriptor.Name
	if deviceName == "" {
		deviceName = "Game controller"
	}
	metadata := fmt.Sprintf(
		"%s  ·  %s  ·  %s  ·  audit %s",
		m.descriptor.Type,
		m.descriptor.Backend,
		m.descriptor.Transport,
		m.descriptor.Capability.AuditGrade,
	)
	if m.auditPath != "" {
		metadata += "  ·  " + filepath.Base(m.auditPath)
	}

	return lipgloss.JoinVertical(
		lipgloss.Left,
		padBetween(title, status, width),
		lipgloss.NewStyle().Bold(true).Render(deviceName),
		mutedStyle.MaxWidth(width).Render(metadata),
		mutedStyle.Render(strings.Repeat("─", width)),
	)
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

func padBetween(left, right string, width int) string {
	spaces := width - lipgloss.Width(left) - lipgloss.Width(right)
	if spaces < 1 {
		spaces = 1
	}
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
	case teleop.EventDPad:
		return "d-pad"
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
	case teleop.DPadEvent:
		return dpad(value.Current)
	case *teleop.DPadEvent:
		return dpad(value.Current)
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

func dpad(value teleop.DPad) string {
	var directions []string
	if value.Up {
		directions = append(directions, "up")
	}
	if value.Down {
		directions = append(directions, "down")
	}
	if value.Left {
		directions = append(directions, "left")
	}
	if value.Right {
		directions = append(directions, "right")
	}
	if len(directions) == 0 {
		return "center"
	}
	return strings.Join(directions, "+")
}
