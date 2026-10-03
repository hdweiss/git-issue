//go:build unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// show pipes a full issue through the pager when its output goes to a terminal,
// the way `git show` does. The pager here is a script that copies what it is
// handed to a file, so the test can assert the rendering reached it rather than
// the terminal.
func TestShowPagesToTerminal(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	capture := filepath.Join(t.TempDir(), "paged")
	pager := fmt.Sprintf("cat > %s", capture)

	master, slave := openPTY(t)

	// Drain the pty in the background so a write to it can never block the
	// child; nothing is expected there, since the pager takes the output.
	go drain(master)

	cmd := exec.Command(bin, "show", "e9037839d7f8")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TZ="+goldenTZ, "GIT_PAGER="+pager)
	cmd.Stdout, cmd.Stderr = slave, slave
	if err := cmd.Run(); err != nil {
		t.Fatalf("show: %v", err)
	}
	slave.Close()

	paged, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("pager wrote nothing: %v", err)
	}
	// Compared with the escapes taken back out: a paged rendering is measured
	// against the screen behind the pager, so its ids carry the colour a piped
	// one has none of. What has to match is the text.
	want := gitIssue(t, bin, dir, "show", "e9037839d7f8").stdout
	if got := stripANSI(string(paged)); strings.TrimRight(got, "\n") != strings.TrimRight(want, "\n") {
		t.Errorf("pager got:\n%s\nwant:\n%s", got, want)
	}
}

// list pages the same way, and the listing keeps the colour and the
// width-aware layout it would have on the bare terminal: render measures those
// against the screen behind the pager, not against the pipe it is writing to.
func TestListPagesInColour(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	capture := filepath.Join(t.TempDir(), "paged")
	master, slave := openPTY(t)
	setWinsize(t, slave, 100, 40)
	go drain(master)

	cmd := exec.Command(bin, "list")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TZ="+goldenTZ, "GIT_PAGER=cat > "+capture)
	cmd.Stdout, cmd.Stderr = slave, slave
	if err := cmd.Run(); err != nil {
		t.Fatalf("list: %v", err)
	}
	slave.Close()

	paged, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("pager wrote nothing: %v", err)
	}
	if !strings.Contains(string(paged), "\x1b[") {
		t.Errorf("paged listing lost its colour:\n%q", paged)
	}
	// Newest issue at the top, as on the bare terminal. Stripped first: the id
	// is painted in two pieces, its unique prefix and the rest, so the escapes
	// sit in the middle of it.
	first := strings.SplitN(stripANSI(string(paged)), "\n", 2)[0]
	if !strings.Contains(first, "f55f8e0bfdc1") {
		t.Errorf("paged listing is not newest-first, opens with:\n%q", first)
	}
}

// stripANSI removes SGR escapes, so a coloured rendering can be compared with
// the plain text it is a colouring of.
func stripANSI(s string) string {
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			return s
		}
		j := strings.IndexByte(s[i:], 'm')
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+1:]
	}
}

// Off a terminal there is nobody watching a pager, so the rendering goes
// straight to stdout and the configured pager is never run.
func TestShowSkipsPagerOffTerminal(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	capture := filepath.Join(t.TempDir(), "paged")
	got := gitIssueEnv(t, bin, dir, []string{"GIT_PAGER=cat > " + capture},
		"show", "e9037839d7f8")
	if got.code != 0 {
		t.Fatalf("show: exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "issue e9037839d7f8") {
		t.Errorf("rendering did not reach stdout:\n%s", got.stdout)
	}
	if _, err := os.Stat(capture); err == nil {
		t.Errorf("the pager ran for output that was not going to a terminal")
	}
}

func drain(f *os.File) {
	buf := make([]byte, 4096)
	for {
		if _, err := f.Read(buf); err != nil {
			return
		}
	}
}

// openPTY allocates a pseudo-terminal pair the same way git's own tests reach
// for one: /dev/ptmx, unlocked and named through the two ioctls Linux and the
// BSDs share. The master is returned so the caller can close it; the slave is
// what a child's stdout attaches to.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}

	var unlock int
	if err := ioctl(m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		m.Close()
		t.Skipf("TIOCSPTLCK: %v", err)
	}
	var n uint32
	if err := ioctl(m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		m.Close()
		t.Skipf("TIOCGPTN: %v", err)
	}

	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Skipf("open pts: %v", err)
	}
	t.Cleanup(func() { s.Close(); m.Close() })
	return m, s
}

// setWinsize gives the pty a fixed size, so a listing paged through it
// truncates at a known width instead of at whatever the pty defaulted to.
func setWinsize(t *testing.T, f *os.File, cols, rows uint16) {
	t.Helper()
	ws := struct{ rows, cols, x, y uint16 }{rows, cols, 0, 0}
	if err := ioctl(f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); err != nil {
		t.Skipf("TIOCSWINSZ: %v", err)
	}
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); errno != 0 {
		return errno
	}
	return nil
}
