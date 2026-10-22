package doctor

import (
	"os"
	"syscall"
)

func diskFree(path string) (int64, int64, error) {
	for {
		var st syscall.Statfs_t
		err := syscall.Statfs(path, &st)
		if err == nil {
			bs := uint64(st.Bsize)
			return int64(uint64(st.Bavail) * bs), int64(uint64(st.Blocks) * bs), nil
		}
		if !os.IsNotExist(err) || path == "/" {
			return 0, 0, err
		}
		// The state dir may not exist yet; measure its nearest parent.
		path = parentOf(path)
	}
}

func parentOf(p string) string {
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "/"
}
