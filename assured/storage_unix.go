//go:build !windows

package assured

import (
	"os"
	"path/filepath"
)

func syncParentDirectory(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
