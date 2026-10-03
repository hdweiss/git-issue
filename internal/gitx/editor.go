package gitx

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Editor is the editor git itself would run: GIT_EDITOR, then core.editor,
// then VISUAL, then EDITOR, then git's compiled-in default.
//
// Asked of git rather than reassembled here, so that a repository's
// core.editor, an include, and a conditional include all mean what they mean
// everywhere else.
func (r *Repo) Editor() (string, error) {
	out, err := r.run(nil, "var", "GIT_EDITOR")
	if err != nil {
		return "", err
	}
	editor := strings.TrimSpace(string(out))
	if editor == "" {
		return "", fmt.Errorf("git has no editor configured")
	}
	return editor, nil
}

// EditorChosen reports whether an editor was chosen deliberately, as opposed
// to git falling back to its compiled-in default.
//
// The distinction is what makes an editor safe to open when nothing is
// attached to a terminal: a script that sets GIT_EDITOR means it, while
// launching a default vi against a pipe would hang a command that had no
// business needing a person.
func (r *Repo) EditorChosen() bool {
	for _, v := range []string{
		os.Getenv("GIT_EDITOR"),
		r.ConfigDefault("core.editor"),
		os.Getenv("VISUAL"),
		os.Getenv("EDITOR"),
	} {
		if v != "" {
			return true
		}
	}
	return false
}

// CanEdit reports whether opening an editor could work: either someone chose
// one, or there is a terminal to run git's default in.
func (r *Repo) CanEdit() bool { return r.EditorChosen() || IsTerminal(os.Stdin) }

// EditFile opens path in the editor and waits for it to close.
//
// The editor string is a command line, not a program name — `code --wait` and
// `emacsclient -a ""` are both ordinary answers — so it goes through a shell
// exactly as git runs it, with the path as an argument rather than pasted into
// the command, so a path containing a space survives.
func (r *Repo) EditFile(path string) error {
	if !r.CanEdit() {
		return fmt.Errorf("no editor to open: set core.editor, or run this where a terminal is attached")
	}
	editor, err := r.Editor()
	if err != nil {
		return err
	}

	cmd := exec.Command("sh", "-c", editor+` "$@"`, editor, path)
	cmd.Dir = r.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor '%s' exited with an error: %s", editor, err)
	}
	return nil
}

// EditFileAt opens path in the editor with the cursor on line, and waits for it
// to close.
//
// There is no standard for this and there is a convention: `<editor> +<line>
// <file>`, which vi established and vim, nvim, emacs, nano, kakoune, micro, joe
// and gedit all still accept. It is what the surrounding ecosystem passes
// (fzf's execute bindings, grep-to-editor wrappers), so it is what an editor
// this does not recognise is given — at worst the cursor lands at the top of the
// right file. The handful that spell it their own way are listed in lineArgs.
//
// cmdline overrides all of that: a command line with %f for the path and %l for
// the line, as in `code -g %f:%l` or `kate -l %l %f`. It is how an editor this
// build has never heard of, or a wrapper of somebody's own, is driven. The
// placeholders become shell positional parameters rather than pasted text, so a
// path with a space in it survives.
func (r *Repo) EditFileAt(path string, line int, cmdline string) error {
	if line <= 0 && cmdline == "" {
		return r.EditFile(path)
	}
	if !r.CanEdit() {
		return fmt.Errorf("no editor to open: set core.editor, or run this where a terminal is attached")
	}

	name, script, args := cmdline, "", []string{}
	if cmdline != "" {
		script = strings.ReplaceAll(cmdline, "%f", `"$1"`)
		script = strings.ReplaceAll(script, "%l", `"$2"`)
		// A command line that names neither placeholder still has to be given
		// the file, or it opens nothing at all.
		if script == cmdline {
			script += ` "$1"`
		}
		args = []string{path, strconv.Itoa(line)}
	} else {
		editor, err := r.Editor()
		if err != nil {
			return err
		}
		name, script, args = editor, editor+` "$@"`, lineArgs(editor, path, line)
	}

	cmd := exec.Command("sh", append([]string{"-c", script, name}, args...)...)
	cmd.Dir = r.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor '%s' exited with an error: %s", name, err)
	}
	return nil
}

// lineArgs is how one editor is asked to open a file with the cursor on a line.
//
// Keyed on the program the command line starts with, so `code --wait` and
// `nvim -p` are recognised as the editors they are. Anything else — including a
// command line that opens with `env` or a wrapper script — gets the `+<line>`
// convention, which is the answer that is right far more often than it is
// wrong.
func lineArgs(editor, path string, line int) []string {
	n := strconv.Itoa(line)
	switch editorName(editor) {
	case "code", "code-insiders", "codium", "vscodium", "cursor", "windsurf":
		return []string{"-g", path + ":" + n}
	case "subl", "sublime_text", "zed", "hx", "helix", "atom", "pulsar":
		return []string{path + ":" + n}
	case "kate", "mate", "gnome-text-editor":
		return []string{"-l", n, path}
	case "idea", "goland", "webstorm", "pycharm", "phpstorm", "clion", "rubymine", "rider":
		return []string{"--line", n, path}
	default:
		return []string{"+" + n, path}
	}
}

// editorName is the program an editor command line runs: its first word,
// without the directory it was found in.
//
// Split on whitespace rather than parsed as shell, so a quoted path with a
// space in it is not recognised and falls through to the `+<line>` convention.
// That is the safe direction to be wrong in, and the alternative is a shell
// parser in aid of a lookup table.
func editorName(editor string) string {
	fields := strings.Fields(editor)
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(strings.Trim(fields[0], `"'`))
}
