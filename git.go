package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kova1max/diffskim/internal/patch"
)

// gitDiffArgs are added to every git diff diffskim runs, so the output is a
// plain patch whatever the user's git config says.
var gitDiffArgs = []string{
	"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-relative",
	"--src-prefix=a/", "--dst-prefix=b/",
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// baselineArgs builds the git built-ins baseline that the summary compares
// against: renames detected, whitespace ignored, lockfiles excluded. Paths
// the user gave without "--" are moved after it, where the excludes go,
// unless an option is given: its value could look like a path.
func baselineArgs(userArgs []string) []string {
	revs, paths := userArgs, []string(nil)
	for i, a := range userArgs {
		if a == "--" {
			revs, paths = userArgs[:i], userArgs[i+1:]
			break
		}
	}
	var bare []string
	hasOption := slices.ContainsFunc(revs, func(a string) bool { return strings.HasPrefix(a, "-") })
	if out, err := gitOutput("", append([]string{"rev-parse", "--no-revs", "--no-flags"}, revs...)...); err == nil && !hasOption {
		bare = strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	}
	var kept []string
	for _, a := range revs {
		if slices.Contains(bare, a) {
			paths = append(paths, a)
		} else {
			kept = append(kept, a)
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	args := append(append([]string{}, gitDiffArgs...), "-M", "-w")
	args = append(append(args, kept...), "--")
	args = append(args, paths...)
	for _, name := range lockfileExcludes() {
		args = append(args, name)
	}
	return args
}

// baselineChanged counts the changed lines of the baseline, or returns -1
// when git cannot produce it.
func baselineChanged(userArgs []string) int {
	out, err := gitOutput("", baselineArgs(userArgs)...)
	if err != nil {
		return -1
	}
	p, err := patch.Parse(string(out))
	if err != nil {
		return -1
	}
	return p.Changed()
}

// repo answers questions about the git repository in the current directory.
type repo struct {
	top       string
	generated map[string]bool

	batch *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Reader
}

// openRepo returns nil when the current directory is not in a git work tree.
func openRepo() *repo {
	top, err := gitOutput("", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	}
	return &repo{top: strings.TrimSpace(string(top))}
}

func (r *repo) close() {
	if r != nil && r.batch != nil {
		r.in.Close()
		r.batch.Wait()
	}
}

// loadAttributes reads linguist-generated for every path in one git call.
func (r *repo) loadAttributes(p *patch.Patch) {
	r.generated = map[string]bool{}
	var in bytes.Buffer
	for _, f := range p.Files {
		in.WriteString(f.Path())
		in.WriteByte(0)
	}
	cmd := exec.Command("git", "check-attr", "-z", "--stdin", "linguist-generated")
	cmd.Dir = r.top
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		return
	}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+2 < len(fields); i += 3 {
		if v := fields[i+2]; v == "set" || v == "true" {
			r.generated[fields[i]] = true
		}
	}
}

func (r *repo) LinguistGenerated(path string) bool { return r.generated[path] }

// Content reads a file side from the object database by the hash on the
// patch's index line. A new side that is not in the database is usually the
// work tree, so the work tree file is used when it hashes the same.
func (r *repo) Content(f *patch.File, newSide bool) ([]byte, bool) {
	hash, path := f.OldHash, f.OldPath
	if newSide {
		hash, path = f.NewHash, f.NewPath
	}
	if hash == "" || strings.Trim(hash, "0") == "" {
		return nil, false
	}
	if b, ok := r.blob(hash); ok {
		return b, true
	}
	if !newSide {
		return nil, false
	}
	full := filepath.Join(r.top, filepath.FromSlash(path))
	h, err := gitOutput(r.top, "hash-object", "--", full)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(h)), hash) {
		return nil, false
	}
	b, err := os.ReadFile(full)
	return b, err == nil
}

func (r *repo) blob(hash string) ([]byte, bool) {
	if r.batch == nil {
		r.batch = exec.Command("git", "cat-file", "--batch")
		r.batch.Dir = r.top
		var err error
		if r.in, err = r.batch.StdinPipe(); err != nil {
			return nil, false
		}
		stdout, err := r.batch.StdoutPipe()
		if err != nil {
			return nil, false
		}
		r.out = bufio.NewReader(stdout)
		if err := r.batch.Start(); err != nil {
			r.batch = nil
			return nil, false
		}
	}
	if _, err := fmt.Fprintln(r.in, hash); err != nil {
		return nil, false
	}
	header, err := r.out.ReadString('\n')
	if err != nil {
		return nil, false
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[1] != "blob" {
		return nil, false // "<hash> missing" or "<hash> ambiguous"
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil {
		return nil, false
	}
	b := make([]byte, size+1) // content plus a newline
	if _, err := io.ReadFull(r.out, b); err != nil {
		return nil, false
	}
	return b[:size], true
}
