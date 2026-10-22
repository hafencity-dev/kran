package agents

import (
	"os"
	"path/filepath"

	"github.com/hafencity-dev/kran/internal/config"
)

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	return config.WriteFileAtomic(path, data, perm)
}

// resolve follows symlinks so deletes and writes hit the real file.
func resolve(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}
