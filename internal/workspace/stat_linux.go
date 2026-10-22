package workspace

import (
	"os"
	"syscall"
)

func statKeyOf(fi os.FileInfo) statKey {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return statKey{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	}
	return statKey{
		dev:   uint64(st.Dev),
		ino:   uint64(st.Ino),
		size:  st.Size,
		mtime: st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		ctime: st.Ctim.Sec*1e9 + st.Ctim.Nsec,
	}
}
