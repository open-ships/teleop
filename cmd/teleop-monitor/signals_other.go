//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package main

import "os"

func monitoredSignals() []os.Signal { return []os.Signal{os.Interrupt} }
