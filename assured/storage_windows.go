//go:build windows

package assured

// Windows FlushFileBuffers on the newly created file is the portable
// durability primitive exposed by os.File. Opening a directory for an
// additional flush requires platform privileges/filesystem behavior that a
// generic library cannot require.
func syncParentDirectory(string) error { return nil }
