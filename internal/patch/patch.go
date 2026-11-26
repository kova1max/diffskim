// Package patch parses and writes unified diffs, keeping every byte it does
// not understand so that an unchanged patch round-trips exactly.
package patch

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Patch is a parsed unified diff.
type Patch struct {
	Preamble []string // lines before the first file (commit message, mail headers)
	Files    []*File
}

// File is the part of a patch that changes one file.
type File struct {
	Header  []string // raw lines from "diff ..." or "--- " through "+++ "
	OldPath string   // "" when the file is created
	NewPath string   // "" when the file is deleted
	OldHash string   // from the "index" line, "" when absent
	NewHash string
	// Opaque files (binary patches, combined diffs) keep their body in Body
	// verbatim and have no Hunks.
	Opaque bool
	Body   []string
	Hunks  []*Hunk
	// Trailer holds lines after the last hunk that belong to no file, such
	// as the next commit's message in "git log -p" output.
	Trailer []string
}

// Path is the path the change is best known by: the new path, or the old
// one for a deletion.
func (f *File) Path() string {
	if f.NewPath != "" {
		return f.NewPath
	}
	return f.OldPath
}

// Hunk is one "@@ -a,b +c,d @@" section.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Heading            string // text after the closing "@@", with its leading space
	Lines              []Line
}

// Line is one hunk line.
type Line struct {
	Kind byte   // ' ', '-' or '+'
	Text string // without the kind prefix
	// NoEOL holds the "\ No newline at end of file" marker that follows this
	// line, or "" when there is none.
	NoEOL string
}

// Changed counts the added and removed lines of the hunk.
func (h *Hunk) Changed() int {
	n := 0
	for _, l := range h.Lines {
		if l.Kind != ' ' {
			n++
		}
	}
	return n
}

// Changed counts the added and removed lines of the file.
func (f *File) Changed() int {
	n := 0
	for _, h := range f.Hunks {
		n += h.Changed()
	}
	return n
}

// Changed counts the added and removed lines of the patch.
func (p *Patch) Changed() int {
	n := 0
	for _, f := range p.Files {
		n += f.Changed()
	}
	return n
}

// Parse reads a unified diff: git diff output, plain "diff -u" output, or
// either one with a commit message or mail headers in front.
func Parse(text string) (*Patch, error) {
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\n")
	}
	if len(lines) > 0 && !strings.HasSuffix(text, "\n") {
		return nil, fmt.Errorf("input does not end with a newline")
	}
	p := &Patch{}
	var cur *File
	for i := 0; i < len(lines); {
		if fileStart(lines, i) {
			f, next, err := parseFile(lines, i)
			if err != nil {
				return nil, err
			}
			p.Files = append(p.Files, f)
			cur = f
			i = next
			continue
		}
		if cur == nil {
			p.Preamble = append(p.Preamble, lines[i])
		} else {
			cur.Trailer = append(cur.Trailer, lines[i])
		}
		i++
	}
	return p, nil
}

func fileStart(lines []string, i int) bool {
	l := lines[i]
	if strings.HasPrefix(l, "diff ") {
		return true
	}
	return strings.HasPrefix(l, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ")
}

// extendedHeaders are the git header lines that may follow "diff --git".
var extendedHeaders = []string{
	"old mode ", "new mode ", "deleted file mode ", "new file mode ",
	"copy from ", "copy to ", "rename from ", "rename to ",
	"similarity index ", "dissimilarity index ", "index ",
}

func parseFile(lines []string, i int) (*File, int, error) {
	f := &File{}
	git := strings.HasPrefix(lines[i], "diff --git ")
	if strings.HasPrefix(lines[i], "diff --cc ") || strings.HasPrefix(lines[i], "diff --combined ") {
		// Combined diffs of merge commits pass through untouched.
		f.Opaque = true
		f.Header = []string{lines[i]}
		f.NewPath = lines[i][strings.Index(lines[i][5:], " ")+6:]
		i++
		for i < len(lines) && !strings.HasPrefix(lines[i], "diff ") {
			f.Body = append(f.Body, lines[i])
			i++
		}
		return f, i, nil
	}
	if strings.HasPrefix(lines[i], "diff ") {
		f.Header = append(f.Header, lines[i])
		if git {
			f.OldPath, f.NewPath = gitHeaderPaths(lines[i])
		}
		i++
		for i < len(lines) && hasAnyPrefix(lines[i], extendedHeaders) {
			l := lines[i]
			f.Header = append(f.Header, l)
			switch {
			case strings.HasPrefix(l, "index "):
				f.OldHash, f.NewHash = indexHashes(l)
			case strings.HasPrefix(l, "rename from "), strings.HasPrefix(l, "copy from "):
				f.OldPath = unquote(l[strings.Index(l, "from ")+5:])
			case strings.HasPrefix(l, "rename to "), strings.HasPrefix(l, "copy to "):
				f.NewPath = unquote(l[strings.Index(l, "to ")+3:])
			case strings.HasPrefix(l, "new file mode "):
				f.OldPath = ""
			case strings.HasPrefix(l, "deleted file mode "):
				f.NewPath = ""
			}
			i++
		}
	}
	if i < len(lines) && (strings.HasPrefix(lines[i], "Binary files ") || lines[i] == "GIT binary patch") {
		f.Opaque = true
		for i < len(lines) && !strings.HasPrefix(lines[i], "diff ") {
			f.Body = append(f.Body, lines[i])
			i++
		}
		return f, i, nil
	}
	if i+1 < len(lines) && strings.HasPrefix(lines[i], "--- ") && strings.HasPrefix(lines[i+1], "+++ ") {
		f.Header = append(f.Header, lines[i], lines[i+1])
		f.OldPath = markerPath(lines[i][4:], git)
		f.NewPath = markerPath(lines[i+1][4:], git)
		i += 2
	}
	for i < len(lines) && strings.HasPrefix(lines[i], "@@ ") {
		h, next, err := parseHunk(lines, i)
		if err != nil {
			return nil, 0, err
		}
		f.Hunks = append(f.Hunks, h)
		i = next
	}
	return f, i, nil
}

func parseHunk(lines []string, i int) (*Hunk, int, error) {
	h := &Hunk{}
	var err error
	h.OldStart, h.OldLines, h.NewStart, h.NewLines, h.Heading, err = parseHunkHeader(lines[i])
	if err != nil {
		return nil, 0, fmt.Errorf("line %d: %v", i+1, err)
	}
	i++
	oldLeft, newLeft := h.OldLines, h.NewLines
	for oldLeft > 0 || newLeft > 0 {
		if i >= len(lines) {
			return nil, 0, fmt.Errorf("line %d: hunk ends early", i)
		}
		l := lines[i]
		kind := byte(' ')
		text := ""
		if l != "" { // some tools strip the space of an empty context line
			kind, text = l[0], l[1:]
		}
		switch {
		case kind == ' ' && oldLeft > 0 && newLeft > 0:
			oldLeft--
			newLeft--
		case kind == '-' && oldLeft > 0:
			oldLeft--
		case kind == '+' && newLeft > 0:
			newLeft--
		case kind == '\\' && len(h.Lines) > 0:
			h.Lines[len(h.Lines)-1].NoEOL = l
			i++
			continue
		default:
			return nil, 0, fmt.Errorf("line %d: unexpected line in hunk: %q", i+1, l)
		}
		h.Lines = append(h.Lines, Line{Kind: kind, Text: text})
		i++
	}
	if i < len(lines) && strings.HasPrefix(lines[i], "\\") && len(h.Lines) > 0 {
		h.Lines[len(h.Lines)-1].NoEOL = lines[i]
		i++
	}
	return h, i, nil
}

func parseHunkHeader(l string) (oldStart, oldLines, newStart, newLines int, heading string, err error) {
	rest, ok := strings.CutPrefix(l, "@@ -")
	if !ok {
		return 0, 0, 0, 0, "", fmt.Errorf("bad hunk header %q", l)
	}
	ranges, heading, ok := strings.Cut(rest, " @@")
	if !ok {
		return 0, 0, 0, 0, "", fmt.Errorf("bad hunk header %q", l)
	}
	oldR, newR, ok := strings.Cut(ranges, " +")
	if !ok {
		return 0, 0, 0, 0, "", fmt.Errorf("bad hunk header %q", l)
	}
	if oldStart, oldLines, err = parseRange(oldR); err != nil {
		return 0, 0, 0, 0, "", fmt.Errorf("bad hunk header %q", l)
	}
	if newStart, newLines, err = parseRange(newR); err != nil {
		return 0, 0, 0, 0, "", fmt.Errorf("bad hunk header %q", l)
	}
	return oldStart, oldLines, newStart, newLines, heading, nil
}

func parseRange(r string) (start, n int, err error) {
	s, c, hasCount := strings.Cut(r, ",")
	if start, err = strconv.Atoi(s); err != nil {
		return 0, 0, err
	}
	n = 1
	if hasCount {
		n, err = strconv.Atoi(c)
	}
	return start, n, err
}

// gitHeaderPaths splits "diff --git a/x b/y". It is only a fallback for files
// without "---"/"+++" lines, so an ambiguous split of unquoted paths with
// spaces just guesses that both sides are equal.
func gitHeaderPaths(l string) (string, string) {
	rest := l[len("diff --git "):]
	if strings.HasPrefix(rest, `"`) {
		if end := closingQuote(rest); end > 0 {
			return stripPrefix(unquote(rest[:end+1])), stripPrefix(unquote(strings.TrimSpace(rest[end+1:])))
		}
	}
	if half := (len(rest) - 1) / 2; len(rest)%2 == 1 && rest[half] == ' ' && stripPrefix(rest[:half]) == stripPrefix(rest[half+1:]) {
		return stripPrefix(rest[:half]), stripPrefix(rest[half+1:])
	}
	if a, b, ok := strings.Cut(rest, " b/"); ok {
		return stripPrefix(a), b
	}
	return "", ""
}

func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// markerPath reads the path of a "---" or "+++" line.
func markerPath(s string, git bool) string {
	if t, _, ok := strings.Cut(s, "\t"); ok && !strings.HasPrefix(s, `"`) {
		s = t // plain diff -u appends a tab and a timestamp
	}
	if s == "/dev/null" {
		return ""
	}
	s = unquote(s)
	if git {
		return stripPrefix(s)
	}
	return s
}

// stripPrefix drops git's "a/", "b/" or mnemonic ("i/", "w/", ...) prefix.
func stripPrefix(s string) string {
	if len(s) > 2 && s[1] == '/' && strings.IndexByte("abciwo12", s[0]) >= 0 {
		return s[2:]
	}
	return s
}

func unquote(s string) string {
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

func indexHashes(l string) (string, string) {
	r := strings.Fields(l[len("index "):])
	if len(r) == 0 {
		return "", ""
	}
	a, b, _ := strings.Cut(r[0], "..")
	return a, b
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Write prints the patch in unified diff format.
func (p *Patch) Write(w io.Writer) error {
	var b strings.Builder
	put := func(ls []string) {
		for _, l := range ls {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	put(p.Preamble)
	for _, f := range p.Files {
		put(f.Header)
		put(f.Body)
		for _, h := range f.Hunks {
			b.WriteString(h.header())
			b.WriteByte('\n')
			for _, l := range h.Lines {
				b.WriteByte(l.Kind)
				b.WriteString(l.Text)
				b.WriteByte('\n')
				if l.NoEOL != "" {
					b.WriteString(l.NoEOL)
					b.WriteByte('\n')
				}
			}
		}
		put(f.Trailer)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func (h *Hunk) header() string {
	return "@@ -" + formatRange(h.OldStart, h.OldLines) + " +" + formatRange(h.NewStart, h.NewLines) + " @@" + h.Heading
}

func formatRange(start, n int) string {
	if n == 1 {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(n)
}
