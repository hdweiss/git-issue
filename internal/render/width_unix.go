//go:build unix

package render

import (
	"os"
	"syscall"
	"unsafe"
)

// terminalWidth asks the kernel how many columns f is displaying, or returns 0
// when f is not a terminal.
//
// This is the ioctl git itself uses. Doing it by hand rather than through
// golang.org/x/term keeps the tree on the standard library, which is the same
// call embedding tzdata made: a single static binary that assumes nothing
// about its host.
func terminalWidth(f *os.File) int {
	var ws struct{ rows, cols, xpixel, ypixel uint16 }
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.cols)
}
