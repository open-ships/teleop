//go:build linux && !(ppc || ppc64 || ppc64le || mips || mipsle || mips64 || mips64le)

package xbox

// linuxIOR constructs an _IOR request using Linux's asm-generic ioctl layout.
func linuxIOR(kind, number, size uintptr) uintptr {
	const (
		read           = 2
		directionShift = 30
		sizeShift      = 16
		typeShift      = 8
	)
	return uintptr(read)<<directionShift | size<<sizeShift | kind<<typeShift | number
}
