//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// listOnPTY runs `git issue <args>` with stdout on a pseudo-terminal and its
// pager set to a script that copies what it is handed to a file, so a test can
// read back exactly what a terminal would have shown.
func listOnPTY(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	capture := filepath.Join(t.TempDir(), "paged")
	master, slave := openPTY(t)
	setWinsize(t, slave, 100, 40)
	go drain(master)

	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TZ="+goldenTZ, "GIT_PAGER=cat > "+capture)
	cmd.Stdout, cmd.Stderr = slave, slave
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	slave.Close()

	paged, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("pager wrote nothing for %v: %v", args, err)
	}
	return stripANSI(string(paged))
}

// A oneline listing nests on a terminal without anyone asking: the spine is
// what a person reads, so it is the default there. --no-tree opts back out.
func TestListNestsOnTerminal(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	_, _, filters, _, _ := hierarchy(t, bin, dir)

	nested := listOnPTY(t, bin, dir, "list")
	if !strings.Contains(nested, "└─ Raw-state filters") {
		t.Errorf("a terminal listing did not nest:\n%s", nested)
	}

	flat := listOnPTY(t, bin, dir, "list", "--no-tree")
	if strings.ContainsAny(flat, "├└│") {
		t.Errorf("--no-tree still drew a spine:\n%s", flat)
	}
	if !strings.Contains(flat, filters) {
		t.Errorf("--no-tree dropped a row:\n%s", flat)
	}
}

// The positional selects the same subtree on a terminal as in a pipe; only the
// shape differs. On a terminal the named issue heads its own tree.
func TestListUnderIssueNestsOnTerminal(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	epic, bug, filters, push, _ := hierarchy(t, bin, dir)

	nested := listOnPTY(t, bin, dir, "list", epic)
	for _, want := range []string{epic, bug, push, filters} {
		if !strings.Contains(nested, want) {
			t.Errorf("%s missing from the epic's tree:\n%s", want, nested)
		}
	}
	if !strings.Contains(nested, "└─ Raw-state filters") && !strings.Contains(nested, "├─ Raw-state filters") {
		t.Errorf("the subtree was not nested:\n%s", nested)
	}
}
