//go:build !unix

package render

import "os"

// terminalWidth has no portable answer off unix, and 0 is the safe one: it
// reads as "no limit", so a listing prints in full rather than being truncated
// against a width nobody measured.
func terminalWidth(f *os.File) int { return 0 }
