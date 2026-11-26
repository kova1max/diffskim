package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoWithChanges makes a git repository with one commit and uncommitted
// changes of every kind diffskim folds, plus one real change, and changes
// into it for the rest of the test.
func repoWithChanges(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, text string) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("src/user.ts", `import { db } from './db'
import { log } from './log'

export function isAdmin(user: { role: string }) {
  return user.role === 'admin'
}

export function greet(name: string) {
  log('hello ' + name)
}
`)
	write("yarn.lock", "left-pad@1:\n  version 1.0.0\n")
	write("api/user.pb.go", "package api\nvar V = 1\n")
	git("add", ".")
	git("commit", "-qm", "init")
	write("src/user.ts", `import { log } from './log';
import { db } from './db';

export function isAdmin(user: { role: string }) {
  return user.role !== 'guest';
}

export function greet(name: string) {
  log("hello " + name);
}
`)
	write("yarn.lock", "left-pad@1:\n  version 1.3.0\n")
	write("api/user.pb.go", "package api\nvar V = 2\n")
	t.Chdir(dir)
	return dir
}

func runCLI(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	in.WriteString(stdin)
	in.Seek(0, 0)
	defer in.Close()
	var out, errOut bytes.Buffer
	code := run(args, in, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestGitMode(t *testing.T) {
	dir := repoWithChanges(t)
	out, errOut, code := runCLI(t, "", "HEAD")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "yarn.lock") || strings.Contains(out, "user.pb.go") || strings.Contains(out, "\n+import") || strings.Contains(out, "\n-import") {
		t.Errorf("folded content left in the output:\n%s", out)
	}
	if !strings.Contains(out, "+  return user.role !== 'guest';") {
		t.Errorf("real change missing from the output:\n%s", out)
	}
	if !strings.Contains(out, "@@ [folded above: generated 2 lines, import-only 4 lines]") {
		t.Errorf("fold note missing from the hunk heading:\n%s", out)
	}
	want := "diffskim: 12 changed lines -> 2 kept (git diff -M -w + lockfile excludes: 10); folded lockfile 2, generated 2, import-only 4, format-only 2\n"
	if errOut != want {
		t.Errorf("summary\n got %q\nwant %q", errOut, want)
	}

	apply := exec.Command("git", "apply", "--check", "--cached", "-")
	apply.Dir = dir
	apply.Stdin = strings.NewReader(out)
	if msg, err := apply.CombinedOutput(); err != nil {
		t.Errorf("output does not apply to HEAD: %v\n%s", err, msg)
	}
}

func TestGitModeWithPathsWithoutDoubleDash(t *testing.T) {
	repoWithChanges(t)
	for _, args := range [][]string{{"HEAD", "src/user.ts"}, {"src/user.ts"}} {
		out, errOut, code := runCLI(t, "", args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
		if !strings.Contains(out, "+  return user.role !== 'guest';") || strings.Contains(out, "user.pb.go") {
			t.Errorf("%v: output not limited to src/user.ts:\n%s", args, out)
		}
		want := "diffskim: 8 changed lines -> 2 kept (git diff -M -w + lockfile excludes: 8); folded import-only 4, format-only 2\n"
		if errOut != want {
			t.Errorf("%v: summary\n got %q\nwant %q", args, errOut, want)
		}
	}
}

func TestStdinModeMatchesGitMode(t *testing.T) {
	repoWithChanges(t)
	diff, err := exec.Command("git", "diff", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	fromGit, _, _ := runCLI(t, "", "-q", "HEAD")
	fromStdin, errOut, code := runCLI(t, string(diff), "-q")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// git mode asks for full hashes on the index lines; the hunks must match.
	strip := func(s string) string {
		var keep []string
		for _, l := range strings.Split(s, "\n") {
			if !strings.HasPrefix(l, "index ") {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	if strip(fromGit) != strip(fromStdin) {
		t.Errorf("stdin output differs from git output:\n%s\n---\n%s", fromStdin, fromGit)
	}
}

func TestExplainAndJSON(t *testing.T) {
	repoWithChanges(t)
	out, _, code := runCLI(t, "", "--explain", "-q", "HEAD")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := `api/user.pb.go generated (2 lines): *.pb.go
src/user.ts:1 import-only (4 lines)
src/user.ts:9 format-only (2 lines)
yarn.lock lockfile (2 lines)
`
	if out != want {
		t.Errorf("--explain\n got %q\nwant %q", out, want)
	}

	out, errOut, code := runCLI(t, "", "--json", "HEAD")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var rep struct {
		Changed, Kept int
		Baseline      *int
		Folded        map[string]int
		Folds         []map[string]any
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Changed != 12 || rep.Kept != 2 || rep.Baseline == nil || *rep.Baseline != 10 || rep.Folded["import-only"] != 4 || len(rep.Folds) != 4 {
		t.Errorf("unexpected report: %s", out)
	}
}

func TestUsageAndErrors(t *testing.T) {
	if _, _, code := runCLI(t, "", "--explain", "--json"); code != 2 {
		t.Errorf("--explain --json: exit %d, want 2", code)
	}
	if out, _, code := runCLI(t, "", "--help"); code != 0 || !strings.HasPrefix(out, "usage:") {
		t.Errorf("--help: exit %d, output %q", code, out)
	}
	if _, errOut, code := runCLI(t, "--- a\n+++ b\n@@ -1,3 +1,3 @@\n a\n"); code != 1 || !strings.Contains(errOut, "cannot parse") {
		t.Errorf("bad diff: exit %d, stderr %q", code, errOut)
	}
	t.Chdir(t.TempDir())
	if _, errOut, code := runCLI(t, "", "HEAD"); code != 1 || !strings.Contains(errOut, "not in a git repository") {
		t.Errorf("outside a repository: exit %d, stderr %q", code, errOut)
	}
}

func TestStdinOutsideARepository(t *testing.T) {
	t.Chdir(t.TempDir())
	diff := "--- a/x.ts\n+++ b/x.ts\n@@ -1 +1 @@\n-const a = 'x'\n+const a = \"x\";\n"
	out, errOut, code := runCLI(t, diff)
	if code != 0 || out != diff {
		t.Errorf("exit %d, output %q", code, out)
	}
	if !strings.Contains(errOut, "import/format rules skipped 2 lines") {
		t.Errorf("summary does not say the rules were skipped: %q", errOut)
	}
}
