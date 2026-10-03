//go:build unix

package gitx

import (
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether f is a terminal, by asking the kernel for its
// window size — the same ioctl git uses, and the reason /dev/null does not
// pass for one despite being a character device.
func IsTerminal(f *os.File) bool {
	var ws struct{ rows, cols, xpixel, ypixel uint16 }
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	return errno == 0
}
