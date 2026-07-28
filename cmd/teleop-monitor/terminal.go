package main

import (
	"os"
	"strings"
	"unicode"

	"golang.org/x/term"
)

func terminalOutput() bool {
	return isTerminal(int(os.Stdout.Fd()))
}

func isTerminal(fd int) bool { return term.IsTerminal(fd) }

// terminalText strips terminal-control code points from device- and
// backend-supplied text before it is rendered by the interactive monitor.
func terminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}
