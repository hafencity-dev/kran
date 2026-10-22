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
		ino:   st.Ino,
		size:  st.Size,
		mtime: st.Mtimespec.Sec*1e9 + st.Mtimespec.Nsec,
		ctime: st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec,
	}
}
