package execenv

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFileAt creates name in dirfd with the content of src: a reflink
// (FICLONE) where the filesystem supports it (btrfs, xfs, bcachefs), else a
// kernel-side copy (copy_file_range via io.Copy on *os.File).
func cloneFileAt(src string, dirfd int, name string) error {
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
	if err := unix.IoctlFileClone(fd, int(in.Fd())); err == nil {
		return out.Close()
	} else if !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTTY) && !errors.Is(err, unix.ENOSYS) {
		out.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
