//go:build windows

package main

import "os"

func monitoredSignals() []os.Signal { return []os.Signal{os.Interrupt} }
