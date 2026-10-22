package execenv

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFileAt creates name in dirfd with the content of src: an APFS clone
// (clonefile, copy-on-write, O(1)) when src and the slot share a volume,
// else a plain copy.
func cloneFileAt(src string, dirfd int, name string) error {
	err := unix.Clonefileat(unix.AT_FDCWD, src, dirfd, name, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.EOPNOTSUPP) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	out := os.NewFile(uintptr(fd), name)
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
