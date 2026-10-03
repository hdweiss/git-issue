// Package cli is the command plumbing both git-issue and git-review need and
// neither owns: paging, and nothing else yet.
//
// It sits beside the type packages rather than inside one, because everything
// here is entity-agnostic — a pager does not know what it is paging. Anything
// that would need to know belongs in the command that knows.
package cli

import (
	"io"
	"os"
	"os/exec"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
)

// Pager routes output through the user's pager, the way `git log` and `git
// show` do: a rendering that fits on one screen still lands straight on the
// terminal, a longer one opens in less, and quitting the pager leaves the text
// in the scrollback.
//
// The one-screen behaviour is less's, not ours — the LESS=FRX handed to it
// below means "quit if it fits, keep colour, don't clear on exit". Matching
// git's environment here is deliberate: someone who has tuned core.pager or
// $LESS for git gets the same experience from this.
type Pager struct {
	w    io.Writer
	tty  *os.File // the terminal output ends up on, paged or direct; nil off a terminal
	cmd  *exec.Cmd
	pipe io.WriteCloser
}

// StartPager returns a writer that feeds the pager. When stdout is not a
// terminal, or the configured pager is empty or "cat", or the pager fails to
// start, it writes straight to stdout instead. finish must be called once
// writing is done.
func StartPager(s *entity.Store) *Pager {
	if !gitx.IsTerminal(os.Stdout) {
		return &Pager{w: os.Stdout}
	}

	name := pagerCommand(s)
	if name == "" || name == "cat" {
		return &Pager{w: os.Stdout, tty: os.Stdout}
	}

	// core.pager and $PAGER may carry arguments ("less -S"), so the value is a
	// shell command line, not a bare executable.
	cmd := exec.Command("sh", "-c", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = pagerEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return &Pager{w: os.Stdout, tty: os.Stdout}
	}
	if err := cmd.Start(); err != nil {
		return &Pager{w: os.Stdout, tty: os.Stdout}
	}
	return &Pager{w: stdin, tty: os.Stdout, cmd: cmd, pipe: stdin}
}

// Write forwards to the pager, or to stdout when none is running. A write that
// fails because the reader quit early is reported like any other; callers here
// render best-effort and do not check.
func (p *Pager) Write(b []byte) (int, error) { return p.w.Write(b) }

// Terminal is the screen this output lands on — the real terminal even while
// Write is going to the pager's pipe — or nil when it is not headed for one.
// render uses it to keep a paged listing's colour and column width measured
// against the display rather than against the pipe.
func (p *Pager) Terminal() *os.File { return p.tty }

// Finish closes the pipe so the pager sees EOF, then waits for the user to
// leave it. It is a no-op when no pager was started.
func (p *Pager) Finish() {
	if p.cmd == nil {
		return
	}
	p.pipe.Close()
	p.cmd.Wait()
}

// pagerCommand resolves the pager the same order git does: $GIT_PAGER, then
// core.pager, then $PAGER, then less.
func pagerCommand(s *entity.Store) string {
	if v := os.Getenv("GIT_PAGER"); v != "" {
		return v
	}
	if v, err := s.Repo.Config("core.pager"); err == nil && v != "" {
		return v
	}
	if v := os.Getenv("PAGER"); v != "" {
		return v
	}
	return "less"
}

// pagerEnv is the process environment with the defaults git sets for a pager
// added when the user has not set them: LESS=FRX so less quits on short output,
// keeps colour and does not wipe the screen, and LV=-c for the same colour
// reason under that pager.
func pagerEnv() []string {
	env := os.Environ()
	if os.Getenv("LESS") == "" {
		env = append(env, "LESS=FRX")
	}
	if os.Getenv("LV") == "" {
		env = append(env, "LV=-c")
	}
	return env
}
