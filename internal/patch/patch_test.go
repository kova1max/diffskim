package patch

import (
	"strings"
	"testing"
)

const gitPatch = `From 1234 Mon Sep 17 00:00:00 2001
Subject: [PATCH] example

---
diff --git a/src/app.ts b/src/app.ts
index 1111111..2222222 100644
--- a/src/app.ts
+++ b/src/app.ts
@@ -1,4 +1,4 @@ export function main() {
 one
--- a removed line that looks like a header
+++ an added line that looks like a header
 three
 four
diff --git a/old name.txt b/new name.txt
similarity index 90%
rename from old name.txt
rename to new name.txt
index 3333333..4444444
--- a/old name.txt
+++ b/new name.txt
@@ -1 +1 @@
-a
\ No newline at end of file
+a
diff --git a/logo.png b/logo.png
index 5555555..6666666 100644
Binary files a/logo.png and b/logo.png differ
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 7777777..0000000
--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-x
-y
diff --git "a/caf\303\251.txt" "b/caf\303\251.txt"
new file mode 100644
index 0000000..8888888
--- /dev/null
+++ "b/caf\303\251.txt"
@@ -0,0 +1 @@
+hi
--
2.47.0
`

func TestRoundTrip(t *testing.T) {
	for name, in := range map[string]string{
		"git":      gitPatch,
		"plain":    "--- a.txt\t2026-01-01 00:00:00\n+++ b.txt\t2026-01-02 00:00:00\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n",
		"empty":    "",
		"combined": "diff --cc merged.txt\nindex 1,2..3\n--- a/merged.txt\n+++ b/merged.txt\n@@@ -1,1 -1,1 +1,1 @@@\n- a\n -b\n++c\n",
	} {
		t.Run(name, func(t *testing.T) {
			p, err := Parse(in)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			if err := p.Write(&b); err != nil {
				t.Fatal(err)
			}
			if b.String() != in {
				t.Errorf("round trip changed the patch:\n%s", b.String())
			}
		})
	}
}

func TestParse(t *testing.T) {
	p, err := Parse(gitPatch)
	if err != nil {
		t.Fatal(err)
	}
	type file struct {
		oldPath, newPath, oldHash, newHash string
		opaque                             bool
		changed                            int
	}
	want := []file{
		{"src/app.ts", "src/app.ts", "1111111", "2222222", false, 2},
		{"old name.txt", "new name.txt", "3333333", "4444444", false, 2},
		{"logo.png", "logo.png", "5555555", "6666666", true, 0},
		{"gone.txt", "", "7777777", "0000000", false, 2},
		{"", "café.txt", "0000000", "8888888", false, 1},
	}
	if len(p.Files) != len(want) {
		t.Fatalf("got %d files, want %d", len(p.Files), len(want))
	}
	for i, f := range p.Files {
		got := file{f.OldPath, f.NewPath, f.OldHash, f.NewHash, f.Opaque, f.Changed()}
		if got != want[i] {
			t.Errorf("file %d = %+v, want %+v", i, got, want[i])
		}
	}
	if len(p.Preamble) != 4 {
		t.Errorf("preamble = %q", p.Preamble)
	}
	if h := p.Files[0].Hunks[0]; h.Heading != " export function main() {" {
		t.Errorf("heading = %q", h.Heading)
	}
	if l := p.Files[1].Hunks[0].Lines[0]; l.NoEOL == "" {
		t.Errorf("missing no-newline marker on %+v", l)
	}
	if tr := p.Files[4].Trailer; len(tr) != 2 {
		t.Errorf("trailer = %q", tr)
	}
}

func TestParseErrors(t *testing.T) {
	for name, in := range map[string]string{
		"short hunk":   "--- a\n+++ b\n@@ -1,3 +1,3 @@\n a\n",
		"bad line":     "--- a\n+++ b\n@@ -1,2 +1,2 @@\n a\n?b\n",
		"bad header":   "--- a\n+++ b\n@@ -x +1 @@\n a\n",
		"no final eol": "--- a\n+++ b\n@@ -1 +1 @@\n-a\n+b",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
