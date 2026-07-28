//go:build linux && (ppc || ppc64 || ppc64le || mips || mipsle || mips64 || mips64le)

package xbox

// linuxIOR constructs an _IOR request for Linux architectures whose kernel
// ABI uses a 13-bit size field and puts direction at bit 29.
func linuxIOR(kind, number, size uintptr) uintptr {
	const (
		read           = 2
		directionShift = 29
		sizeShift      = 16
		typeShift      = 8
	)
	return uintptr(read)<<directionShift | size<<sizeShift | kind<<typeShift | number
}
