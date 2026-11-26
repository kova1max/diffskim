package skim

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kova1max/diffskim/internal/patch"
)

// files is a Source that knows both sides of every file by path.
type files struct {
	old, new  map[string]string
	generated map[string]bool
}

func (f files) Content(pf *patch.File, newSide bool) ([]byte, bool) {
	m, p := f.old, pf.OldPath
	if newSide {
		m, p = f.new, pf.NewPath
	}
	s, ok := m[p]
	return []byte(s), ok
}

func (f files) LinguistGenerated(p string) bool { return f.generated[p] }

// diff runs git diff --no-index on old and new versions of one file.
func diff(t *testing.T, path, oldText, newText string) string {
	t.Helper()
	dir := t.TempDir()
	for side, text := range map[string]string{"a": oldText, "b": newText} {
		p := filepath.Join(dir, side, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "diff", "--no-index", "--no-prefix", "--no-color", "a/"+path, "b/"+path)
	cmd.Dir = dir
	out, _ := cmd.Output() // exit status 1 means "differs"
	if len(out) == 0 {
		t.Fatalf("no diff for %s", path)
	}
	return string(out)
}

// skimOne diffs one file, skims it with both sides known, and checks that
// the output still applies to the old side.
func skimOne(t *testing.T, path, oldText, newText string) Result {
	t.Helper()
	p, err := patch.Parse(diff(t, path, oldText, newText))
	if err != nil {
		t.Fatal(err)
	}
	r := Skim(p, files{old: map[string]string{path: oldText}, new: map[string]string{path: newText}})
	checkApplies(t, r.Patch, path, oldText)
	return r
}

func checkApplies(t *testing.T, p *patch.Patch, path, oldText string) {
	t.Helper()
	if len(p.Files) == 0 {
		return
	}
	dir := t.TempDir()
	full := filepath.Join(dir, path)
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte(oldText), 0o644)
	var buf bytes.Buffer
	p.Write(&buf)
	cmd := exec.Command("git", "apply", "--check", "-p1", "-")
	cmd.Dir = dir
	cmd.Stdin = &buf
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("skimmed patch does not apply: %v\n%s\n%s", err, out, buf.String())
	}
}

// rules lists the rules of the folds, once per run of equal rules.
func rules(r Result) string {
	var out []string
	for _, f := range r.Folds {
		if len(out) == 0 || out[len(out)-1] != string(f.Rule) {
			out = append(out, string(f.Rule))
		}
	}
	return strings.Join(out, ",")
}

func TestFolds(t *testing.T) {
	tests := []struct {
		name, path, old, new string
		want                 string // comma-separated rules of the folds, "" for none
	}{
		{
			name: "quotes and semicolons",
			path: "a.ts",
			old:  "const a = 'x'\nfoo(a)\n",
			new:  "const a = \"x\";\nfoo(a);\n",
			want: "format-only",
		},
		{
			name: "escaped quotes",
			path: "a.js",
			old:  "const a = 'it\\'s'\n",
			new:  "const a = \"it's\"\n",
			want: "format-only",
		},
		{
			name: "re-wrapped call with trailing comma",
			path: "a.ts",
			old:  "foo(alpha, beta, gamma)\n",
			new:  "foo(\n  alpha,\n  beta,\n  gamma,\n)\n",
			want: "format-only",
		},
		{
			name: "dropping a semicolon before a parenthesis joins two statements",
			path: "a.js",
			old:  "let a = b;\n(c || d).run()\n",
			new:  "let a = b\n(c || d).run()\n",
		},
		{
			name: "a line break after return changes the return value",
			path: "a.js",
			old:  "function f() {\n  return x;\n}\n",
			new:  "function f() {\n  return;\n  x;\n}\n",
		},
		{
			name: "string content",
			path: "a.ts",
			old:  "const a = 'x'\n",
			new:  "const a = \"y\"\n",
		},
		{
			name: "template literal text re-wrapped",
			path: "a.ts",
			old:  "const q = `select a, b\nfrom t`\n",
			new:  "const q = `select a,\nb from t`\n",
		},
		{
			name: "for clauses moved between separators",
			path: "a.js",
			old:  "for (;i;) {}\n",
			new:  "for (i;;) {}\n",
		},
		{
			name: "comment text",
			path: "a.ts",
			old:  "// retry twice\nrun()\n",
			new:  "// retry once\nrun()\n",
		},
		{
			name: "JSX text re-wrapped",
			path: "a.jsx",
			old:  "const a = (\n  <p>\n    hello there world\n  </p>\n)\n",
			new:  "const a = (\n  <p>\n    hello there\n    world\n  </p>\n)\n",
			want: "format-only",
		},
		{
			name: "JSX text changed",
			path: "a.jsx",
			old:  "const a = <p>hello</p>\n",
			new:  "const a = <p>goodbye</p>\n",
		},
		{
			name: "reordered imports",
			path: "a.ts",
			old:  "import { b } from './b'\nimport { a } from './a'\n\nrun(a, b)\n",
			new:  "import { a } from './a'\nimport { b } from './b'\n\nrun(a, b)\n",
			want: "import-only",
		},
		{
			name: "reordered and added imports",
			path: "a.ts",
			old:  "import { b } from './b'\nimport { a } from './a'\n\nrun(a, b)\n",
			new:  "import { a } from './a'\nimport { b } from './b'\nimport { c } from './c'\n\nrun(a, b)\n",
		},
		{
			name: "removed import",
			path: "a.ts",
			old:  "import { a } from './a'\nimport { b } from './b'\n\nrun(a)\n",
			new:  "import { a } from './a'\n\nrun(a)\n",
		},
		{
			name: "re-wrapped import with bindings reordered",
			path: "a.tsx",
			old:  "import { b, a } from './x'\n\nrun(a, b)\n",
			new:  "import {\n  a,\n  b,\n} from './x';\n\nrun(a, b)\n",
			want: "import-only",
		},
		{
			name: "binding added to a multi-line import",
			path: "a.tsx",
			old:  "import { a, b } from './x'\n\nrun(a, b)\n",
			new:  "import {\n  a,\n  b,\n  c,\n} from './x'\n\nrun(a, b)\n",
		},
		{
			name: "import source swapped",
			path: "a.ts",
			old:  "import { sign } from 'jsonwebtoken'\n\nsign(x)\n",
			new:  "import { sign } from './sign'\n\nsign(x)\n",
		},
		{
			name: "binding renamed",
			path: "a.ts",
			old:  "import { a as b } from './x'\n\nrun(b)\n",
			new:  "import { b as a } from './x'\n\nrun(b)\n",
		},
		{
			name: "side-effect imports reordered",
			path: "a.ts",
			old:  "import './polyfill'\nimport './app'\n",
			new:  "import './app'\nimport './polyfill'\n",
		},
		{
			name: "side-effect import only gained a semicolon",
			path: "a.ts",
			old:  "import { b } from './b'\nimport { a } from './a'\nimport './polyfill'\n",
			new:  "import { a } from './a'\nimport { b } from './b'\nimport './polyfill';\n",
			want: "import-only",
		},
		{
			name: "import next to code",
			path: "a.ts",
			old:  "import { a } from './a'\nrun(a)\n",
			new:  "import { a, b } from './a'\nrun(a, b)\n",
		},
		{
			name: "re-export",
			path: "a.ts",
			old:  "export { a } from './a'\n",
			new:  "export { a, b } from './a'\n",
		},
		{
			name: "Go imports sorted",
			path: "a.go",
			old:  "package a\n\nimport (\n\t\"os\"\n\t\"fmt\"\n)\n\nvar _ = fmt.Sprint(os.Args)\n",
			new:  "package a\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nvar _ = fmt.Sprint(os.Args)\n",
			want: "import-only",
		},
		{
			name: "Go import added",
			path: "a.go",
			old:  "package a\n\nimport (\n\t\"os\"\n\t\"fmt\"\n)\n\nvar _ = fmt.Sprint(os.Args)\n",
			new:  "package a\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"strings\"\n)\n\nvar _ = fmt.Sprint(os.Args)\n",
		},
		{
			name: "Go import source swapped",
			path: "a.go",
			old:  "package a\n\nimport \"crypto/rand\"\n\nvar _ = rand.Read\n",
			new:  "package a\n\nimport \"math/rand\"\n\nvar _ = rand.Read\n",
		},
		{
			name: "Go import grouped",
			path: "a.go",
			old:  "package a\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n",
			new:  "package a\n\nimport (\n\t\"fmt\"\n)\n\nvar _ = fmt.Sprint\n",
			want: "import-only",
		},
		{
			name: "Go blank import",
			path: "a.go",
			old:  "package a\n\nimport (\n\t\"fmt\"\n)\n",
			new:  "package a\n\nimport (\n\t\"fmt\"\n\t_ \"embed\"\n)\n",
		},
		{
			name: "re-indented lines",
			path: "a.c",
			old:  "int f() {\nreturn 1;\n}\n",
			new:  "int f() {\n    return 1;\n}\n",
			want: "whitespace-only",
		},
		{
			name: "space removed inside a string",
			path: "a.ts",
			old:  "const sep = \", \"\n",
			new:  "const sep = \",\"\n",
		},
		{
			name: "space removed between words",
			path: "a.ts",
			old:  "const q = sql`SELECT a FROM b`\n",
			new:  "const q = sql`SELECTa FROMb`\n",
		},
		{
			name: "re-indented TypeScript",
			path: "a.ts",
			old:  "function f() {\nreturn 1\n}\n",
			new:  "function f() {\n  return 1\n}\n",
			want: "format-only",
		},
		{
			name: "space removed inside a C string",
			path: "a.c",
			old:  "const char *sep = \", \";\n",
			new:  "const char *sep = \",\";\n",
		},
		{
			name: "space removed between SQL words",
			path: "a.sql",
			old:  "SELECT a FROM b;\n",
			new:  "SELECTa FROMb;\n",
		},
		{
			name: "runs of spaces collapsed",
			path: "a.sql",
			old:  "SELECT a  FROM\tb;\n",
			new:  "  SELECT a FROM b; \n",
			want: "whitespace-only",
		},
		{
			name: "re-indented Python",
			path: "a.py",
			old:  "if x:\n    a()\nb()\n",
			new:  "if x:\n    a()\n    b()\n",
		},
		{
			name: "re-indented lines next to a real change",
			path: "a.c",
			old:  "int f() {\nint a = 1;\nreturn a;\n}\n",
			new:  "int f() {\n    int a = 2;\n    return a;\n}\n",
			want: "whitespace-only",
		},
		{
			name: "lockfile",
			path: "web/yarn.lock",
			old:  "a@1:\n  version 1\n",
			new:  "a@1:\n  version 2\n",
			want: "lockfile",
		},
		{
			name: "generated by marker",
			path: "api.go",
			old:  "// Code generated by tool. DO NOT EDIT.\npackage api\nvar A = 1\n",
			new:  "// Code generated by tool. DO NOT EDIT.\npackage api\nvar A = 2\n",
			want: "generated",
		},
		{
			name: "generated by name",
			path: "api/v1/user.pb.go",
			old:  "package v1\nvar A = 1\n",
			new:  "package v1\nvar A = 2\n",
			want: "generated",
		},
		{
			name: "real change",
			path: "a.ts",
			old:  "if (user.admin) return true\n",
			new:  "if (user.admin) return false\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := skimOne(t, tt.path, tt.old, tt.new)
			if got := rules(r); got != tt.want {
				t.Errorf("folds = %q, want %q\n%+v", got, tt.want, r.Folds)
			}
			folded := 0
			for _, f := range r.Folds {
				folded += f.Lines
			}
			if r.Kept+folded != r.Changed {
				t.Errorf("kept %d + folded %d != changed %d", r.Kept, folded, r.Changed)
			}
		})
	}
}

func TestMixedBlockKeepsTheRealChange(t *testing.T) {
	r := skimOne(t, "a.c",
		"int f() {\nint a = 1;\nreturn a;\n}\n",
		"int f() {\n    int a = 2;\n    return a;\n}\n")
	var out bytes.Buffer
	r.Patch.Write(&out)
	want := "@@ -1,4 +1,4 @@ [folded here: whitespace-only 2 lines]\n int f() {\n-int a = 1;\n+    int a = 2;\n return a;\n }\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Errorf("got\n%s\nwant suffix\n%s", out.String(), want)
	}
}

func TestFoldNoteGoesToTheNextKeptHunk(t *testing.T) {
	var old, new strings.Builder
	old.WriteString("import { b } from './b'\nimport { a } from './a'\n")
	new.WriteString("import { a } from './a'\nimport { b } from './b'\n")
	for i := 0; i < 20; i++ {
		old.WriteString("run()\n")
		new.WriteString("run()\n")
	}
	old.WriteString("check(true)\n")
	new.WriteString("check(false)\n")
	r := skimOne(t, "a.ts", old.String(), new.String())
	if len(r.Patch.Files) != 1 || len(r.Patch.Files[0].Hunks) != 1 {
		t.Fatalf("want one kept hunk, got %+v", r.Patch.Files)
	}
	h := r.Patch.Files[0].Hunks[0]
	if !strings.HasPrefix(h.Heading, " [folded above: import-only 2 lines]") {
		t.Errorf("heading = %q", h.Heading)
	}
	if h.NewStart != 20 || h.OldStart != 20 {
		t.Errorf("hunk starts at -%d +%d, want the real line numbers -20 +20", h.OldStart, h.NewStart)
	}
}

func TestWithoutContentsOnlyTextRulesRun(t *testing.T) {
	p, err := patch.Parse(diff(t, "a.ts", "const a = 'x'\n", "const a = \"x\";\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := Skim(p, nil)
	if len(r.Folds) != 0 || r.Kept != 2 || r.Unchecked != 2 {
		t.Errorf("folds %v kept %d unchecked %d, want none, 2, 2", r.Folds, r.Kept, r.Unchecked)
	}
}

func TestStaleContentIsNotUsed(t *testing.T) {
	text := diff(t, "a.ts", "const a = 'x'\n", "const a = \"x\";\n")
	p, err := patch.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	src := files{old: map[string]string{"a.ts": "const a = 'x'\n"}, new: map[string]string{"a.ts": "const a = \"y\";\n"}}
	if r := Skim(p, src); len(r.Folds) != 0 {
		t.Errorf("folded with a new side that does not match the patch: %v", r.Folds)
	}
}

func TestLinguistGenerated(t *testing.T) {
	p, err := patch.Parse(diff(t, "gen/schema.ts", "a\n", "b\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := Skim(p, files{generated: map[string]bool{"gen/schema.ts": true}})
	if rules(r) != "generated" || r.Folds[0].Reason != "linguist-generated" || len(r.Patch.Files) != 0 {
		t.Errorf("got %+v", r)
	}
}

func TestMixedBlockKeepsSpaceChangesInsideStrings(t *testing.T) {
	r := skimOne(t, "a.c",
		"int f() {\nputs(\"a, b\");\nreturn 1;\n}\n",
		"int f() {\n    puts(\"a,b\");\n    return 2;\n}\n")
	if len(r.Folds) != 0 || r.Kept != 4 {
		t.Errorf("folds %+v kept %d, want none and 4", r.Folds, r.Kept)
	}
}

func TestFoldedFileKeepsItsTrailer(t *testing.T) {
	log := `commit 2222222222222222222222222222222222222222
Author: t <t@example.com>

    second

diff --git a/a.c b/a.c
--- a/a.c
+++ b/a.c
@@ -1 +1 @@
-int a = 1;
+int a = 2;
diff --git a/yarn.lock b/yarn.lock
--- a/yarn.lock
+++ b/yarn.lock
@@ -1 +1 @@
-a 1
+a 2

commit 1111111111111111111111111111111111111111
Author: t <t@example.com>

    first

diff --git a/yarn.lock b/yarn.lock
--- a/yarn.lock
+++ b/yarn.lock
@@ -1 +1 @@
-a 0
+a 1

commit 0000000000000000000000000000000000000000
Author: t <t@example.com>

    zeroth

diff --git a/b.c b/b.c
--- a/b.c
+++ b/b.c
@@ -1 +1 @@
-int b = 1;
+int b = 2;
`
	p, err := patch.Parse(log)
	if err != nil {
		t.Fatal(err)
	}
	r := Skim(p, nil)
	var out bytes.Buffer
	r.Patch.Write(&out)
	for _, want := range []string{"    second\n", "+int a = 2;\n\ncommit 1111", "    first\n\n\ncommit 0000", "    zeroth\n\ndiff --git a/b.c"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lost %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "yarn.lock") {
		t.Errorf("lockfile left in the output:\n%s", out.String())
	}
	var orig bytes.Buffer
	p.Write(&orig)
	if orig.String() != log {
		t.Errorf("Skim modified its input")
	}
}

func TestFoldedFirstFileKeepsItsTrailer(t *testing.T) {
	log := `commit 1111111111111111111111111111111111111111

    first

diff --git a/yarn.lock b/yarn.lock
--- a/yarn.lock
+++ b/yarn.lock
@@ -1 +1 @@
-a 0
+a 1

commit 0000000000000000000000000000000000000000

    zeroth

diff --git a/b.c b/b.c
--- a/b.c
+++ b/b.c
@@ -1 +1 @@
-int b = 1;
+int b = 2;
`
	p, err := patch.Parse(log)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	Skim(p, nil).Patch.Write(&out)
	want := strings.Replace(log, "diff --git a/yarn.lock b/yarn.lock\n--- a/yarn.lock\n+++ b/yarn.lock\n@@ -1 +1 @@\n-a 0\n+a 1\n", "", 1)
	want = strings.Replace(want, "+++ b/b.c\n@@ -1 +1 @@\n", "+++ b/b.c\n@@ -1 +1 @@ [folded above: lockfile 2 lines]\n", 1)
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
}
