package main

import (
	"os"
	"strings"
	"testing"

	"github.com/open-ships/teleop"
)

func TestTerminalTextRemovesControlSequences(t *testing.T) {
	value := terminalText("Xbox\x1b]8;;https://example.test\x07 controller\n")
	if strings.ContainsRune(value, '\x1b') || strings.ContainsRune(value, '\x07') ||
		strings.ContainsRune(value, '\n') {
		t.Fatalf("terminal text retained a control character: %q", value)
	}
	if value != "Xbox]8;;https://example.test controller" {
		t.Fatalf("terminal text = %q", value)
	}
}

func TestDeviceLineUsesDescriptorNameAndSanitizesIt(t *testing.T) {
	line := deviceLine(teleop.Descriptor{
		ID:        "xbox:\x1b0",
		Name:      "Field controller\n",
		Backend:   "xinput",
		Transport: teleop.TransportUSB,
		Capability: teleop.Capabilities{
			AuditGrade: teleop.AuditSampledState,
		},
	})
	if strings.ContainsAny(line, "\x1b\n") {
		t.Fatalf("device line retained a terminal control character: %q", line)
	}
	for _, fragment := range []string{
		"xbox:0", "Field controller", "backend=xinput", "transport=usb",
		"audit=sampled-state",
	} {
		if !strings.Contains(line, fragment) {
			t.Fatalf("device line %q does not contain %q", line, fragment)
		}
	}
}

func TestDevNullIsNotATerminal(t *testing.T) {
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if isTerminal(int(file.Fd())) {
		t.Fatal("os.DevNull was classified as a terminal")
	}
}
