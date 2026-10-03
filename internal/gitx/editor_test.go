package gitx

import (
	"slices"
	"testing"
)

// The `+<line> <file>` convention is what an unrecognised editor gets, because
// it is what vi established and what most of what came after still takes. The
// editors that spell it their own way are the exceptions, keyed on the program
// the command line runs rather than the whole line.
func TestLineArgsPerEditor(t *testing.T) {
	cases := []struct {
		editor string
		want   []string
	}{
		{"nvim", []string{"+42", "main.go"}},
		{"vim -p", []string{"+42", "main.go"}},
		{"/usr/local/bin/emacs", []string{"+42", "main.go"}},
		{"nano", []string{"+42", "main.go"}},
		// A wrapper this knows nothing about still gets the convention, which
		// is right whenever the wrapper ends in a vi-lineage editor.
		{"env FOO=1 my-editor", []string{"+42", "main.go"}},
		{"code --wait", []string{"-g", "main.go:42"}},
		{"subl -w", []string{"main.go:42"}},
		{"hx", []string{"main.go:42"}},
		{"kate", []string{"-l", "42", "main.go"}},
		{"goland", []string{"--line", "42", "main.go"}},
	}
	for _, c := range cases {
		if got := lineArgs(c.editor, "main.go", 42); !slices.Equal(got, c.want) {
			t.Errorf("lineArgs(%q) = %v, want %v", c.editor, got, c.want)
		}
	}
}

func TestEditorName(t *testing.T) {
	for editor, want := range map[string]string{
		"nvim":               "nvim",
		"code --wait":        "code",
		"/usr/bin/vim -p":    "vim",
		`"/opt/bin/code" -g`: "code",
		"":                   "",
	} {
		if got := editorName(editor); got != want {
			t.Errorf("editorName(%q) = %q, want %q", editor, got, want)
		}
	}
}
