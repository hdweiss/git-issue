//go:build !unix

package gitx

import "os"

// IsTerminal has no portable answer off unix. False is the safe one: it only
// withholds git's default editor from someone who chose no editor at all, and
// says so rather than hanging on a program nobody can see.
func IsTerminal(f *os.File) bool { return false }
