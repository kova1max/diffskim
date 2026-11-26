// Package skim folds away the parts of a patch a reviewer does not need to
// read, and keeps the rest a patch that git apply accepts.
package skim

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kova1max/diffskim/internal/patch"
)

// Source gives the rules access to the repository a patch came from. Both
// methods may always answer "unknown"; the rules that need them then skip.
type Source interface {
	// Content returns the old or new content of a changed file.
	Content(f *patch.File, newSide bool) ([]byte, bool)
	// LinguistGenerated reports whether .gitattributes marks the path
	// linguist-generated.
	LinguistGenerated(path string) bool
}

// Fold is one folded piece of the patch: a whole file or a run of changed
// lines in one hunk.
type Fold struct {
	Path   string `json:"path"`
	Line   int    `json:"line,omitempty"` // first new-side line; 0 for a whole file
	Rule   Rule   `json:"rule"`
	Lines  int    `json:"lines"` // added plus removed lines folded
	Reason string `json:"reason,omitempty"`
}

// Result is a skimmed patch and what was folded out of it.
type Result struct {
	Patch   *patch.Patch
	Folds   []Fold
	Changed int // added plus removed lines in the input
	Kept    int // added plus removed lines in the output
	// Unchecked counts changed lines in JS/TS/Go files that the import and
	// format rules could not check because the file contents were not
	// available.
	Unchecked int
}

// Skim folds p. It does not modify p.
func Skim(p *patch.Patch, src Source) Result {
	s := &skimmer{src: src, r: Result{Patch: &patch.Patch{Preamble: p.Preamble}, Changed: p.Changed()}}
	out := s.r.Patch
	for _, f := range p.Files {
		if kept := s.file(f); kept != nil {
			out.Files = append(out.Files, kept)
		} else if len(out.Files) > 0 {
			last := out.Files[len(out.Files)-1]
			last.Trailer = slices.Concat(last.Trailer, f.Trailer)
		} else {
			out.Preamble = slices.Concat(out.Preamble, f.Trailer)
		}
	}
	s.r.Kept = s.r.Patch.Changed()
	return s.r
}

type skimmer struct {
	src     Source
	r       Result
	pending []Fold // folded since the last kept hunk
}

func (s *skimmer) fold(f Fold) {
	s.r.Folds = append(s.r.Folds, f)
	s.pending = append(s.pending, f)
}

// keep returns h with a note in its heading on the folds before it and the
// whitespace-only lines folded inside it.
func (s *skimmer) keep(h *patch.Hunk, here []Fold) *patch.Hunk {
	var notes []string
	if note := foldNote(s.pending); note != "" {
		notes = append(notes, "[folded above: "+note+"]")
		s.pending = nil
	}
	if note := foldNote(here); note != "" {
		notes = append(notes, "[folded here: "+note+"]")
	}
	if len(notes) > 0 {
		h.Heading = " " + strings.Join(notes, " ") + h.Heading
	}
	return h
}

// foldNote is the text added to a kept hunk's heading for the folds before it.
func foldNote(folds []Fold) string {
	if len(folds) == 0 {
		return ""
	}
	var parts []string
	for _, rule := range Rules {
		n := 0
		for _, f := range folds {
			if f.Rule == rule {
				n += f.Lines
			}
		}
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d %s", rule, n, plural(n, "line")))
		}
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// file returns f as it should be printed, or nil when nothing of it is left.
func (s *skimmer) file(f *patch.File) *patch.File {
	src := s.src
	whole := func(rule Rule, reason string) *patch.File {
		s.fold(Fold{Path: f.Path(), Rule: rule, Lines: f.Changed(), Reason: reason})
		return nil
	}
	if isLockfile(f.Path()) {
		return whole(Lockfile, "")
	}
	if pat := generatedByName(f.Path()); pat != "" {
		return whole(Generated, pat)
	}
	if src != nil && src.LinguistGenerated(f.Path()) {
		return whole(Generated, "linguist-generated")
	}
	if f.Opaque {
		c := *f
		return &c
	}
	oldContent, newContent, haveContent := contents(f, src)
	side := newContent
	if f.NewPath == "" {
		side = oldContent
	}
	if haveContent {
		if m := generatedMarker(side); m != "" {
			return whole(Generated, m)
		}
	} else if len(f.Hunks) > 0 && f.Hunks[0].NewStart <= 1 {
		if m := generatedMarker([]byte(firstLines(f.Hunks[0]))); m != "" {
			return whole(Generated, m)
		}
	}

	lang := language(f.Path())
	var oldA, newA *analysis
	if lang != "" && haveContent {
		oldA, newA = analyze(lang, oldContent), analyze(lang, newContent)
	}
	// Where the format rule can compare syntax trees, it decides alone:
	// whitespace inside a string or between two tokens can matter.
	wsRule := !whitespaceSensitive(f.Path()) && (oldA == nil || newA == nil || lang == "go")
	out := *f
	out.Hunks = nil
	for _, h := range f.Hunks {
		blocks := splitHunk(h)
		for i := range blocks {
			b := &blocks[i]
			b.rule = classify(h, b, wsRule, lang, oldA, newA)
			if b.rule == "" && lang != "" && !haveContent {
				s.r.Unchecked += b.end - b.start
			}
		}
		if !sameImports(h, blocks, oldA, newA) {
			for i := range blocks {
				if blocks[i].rule == Imports {
					blocks[i].rule = ""
				}
			}
		}
		for i := range blocks {
			b := &blocks[i]
			if b.rule == "" && wsRule {
				b.lines, b.wsLines, b.wsLine = foldWhitespaceLines(h.Lines[b.start:b.end], b.newNo[b.start:b.end])
			}
		}
		out.Hunks = append(out.Hunks, s.keptHunks(f, h, blocks)...)
	}
	if len(out.Hunks) == 0 && len(f.Hunks) > 0 && !hasMetadataChange(f) {
		return nil
	}
	return &out
}

// hasMetadataChange reports whether the file header says more than "the
// content changed": a rename, copy, mode change, creation or deletion.
func hasMetadataChange(f *patch.File) bool {
	for _, l := range f.Header {
		for _, p := range []string{"rename from ", "copy from ", "old mode ", "new file mode ", "deleted file mode "} {
			if strings.HasPrefix(l, p) {
				return true
			}
		}
	}
	return false
}

func firstLines(h *patch.Hunk) string {
	var b strings.Builder
	for _, l := range h.Lines {
		if l.Kind != '-' {
			b.WriteString(l.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// contents fetches both sides of f and checks that they match the hunks, so
// a stale or unrelated file can never feed the rules.
func contents(f *patch.File, src Source) (oldC, newC []byte, ok bool) {
	if src == nil {
		return nil, nil, false
	}
	if f.OldPath != "" {
		if oldC, ok = src.Content(f, false); !ok {
			return nil, nil, false
		}
	}
	if f.NewPath != "" {
		if newC, ok = src.Content(f, true); !ok {
			return nil, nil, false
		}
	}
	oldLines, newLines := splitLines(oldC), splitLines(newC)
	for _, h := range f.Hunks {
		o, n := start(h.OldStart, h.OldLines), start(h.NewStart, h.NewLines)
		for _, l := range h.Lines {
			if l.Kind != '+' {
				if o > len(oldLines) || oldLines[o-1] != l.Text {
					return nil, nil, false
				}
				o++
			}
			if l.Kind != '-' {
				if n > len(newLines) || newLines[n-1] != l.Text {
					return nil, nil, false
				}
				n++
			}
		}
	}
	return oldC, newC, true
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(bytes.TrimSuffix(b, []byte("\n"))), "\n")
}

// start is the line number of the first line of a hunk side. A side with no
// lines names the line before the change, so its first line would be next.
func start(s, n int) int {
	if n == 0 {
		return s + 1
	}
	return s
}

// block is one run of changed lines in a hunk, with the context around it.
type block struct {
	start, end   int // the changed lines are h.Lines[start:end]
	ctxStart     int // context before the block starts here (end of the previous block)
	ctxEnd       int // context after the block ends here (start of the next block)
	oldNo, newNo []int
	newLine      int // new-side line number where the block starts
	rule         Rule
	// A kept block may still contain lines that only changed whitespace.
	// lines is then the block with those lines turned into context, wsLines
	// counts the added and removed lines that went, and wsLine is the
	// new-side line number of the first of them.
	lines           []patch.Line
	wsLines, wsLine int
}

func splitHunk(h *patch.Hunk) []block {
	oldNo := make([]int, len(h.Lines)) // line number on the old side, 0 if absent
	newNo := make([]int, len(h.Lines))
	o, n := start(h.OldStart, h.OldLines), start(h.NewStart, h.NewLines)
	var blocks []block
	for i, l := range h.Lines {
		if l.Kind != '+' {
			oldNo[i] = o
			o++
		}
		if l.Kind != '-' {
			newNo[i] = n
			n++
		}
		if l.Kind == ' ' {
			continue
		}
		if len(blocks) > 0 && blocks[len(blocks)-1].end == i {
			blocks[len(blocks)-1].end++
			continue
		}
		blocks = append(blocks, block{start: i, end: i + 1, newLine: n - 1})
		if l.Kind == '-' {
			blocks[len(blocks)-1].newLine = n
		}
	}
	for i := range blocks {
		b := &blocks[i]
		b.oldNo, b.newNo = oldNo, newNo
		if i > 0 {
			b.ctxStart = blocks[i-1].end
		}
		b.ctxEnd = len(h.Lines)
		if i+1 < len(blocks) {
			b.ctxEnd = blocks[i+1].start
		}
	}
	return blocks
}

func classify(h *patch.Hunk, b *block, wsRule bool, lang string, oldA, newA *analysis) Rule {
	var removed, added []string
	var removedNo, addedNo []int
	for i := b.start; i < b.end; i++ {
		l := h.Lines[i]
		if l.Kind == '-' {
			removed = append(removed, l.Text)
			removedNo = append(removedNo, b.oldNo[i])
		} else {
			added = append(added, l.Text)
			addedNo = append(addedNo, b.newNo[i])
		}
	}
	if wsRule && whitespaceOnly(removed, added) {
		return Whitespace
	}
	if oldA == nil || newA == nil {
		return ""
	}
	if importOnly(removed, removedNo, oldA) && importOnly(added, addedNo, newA) &&
		(anyIn(removedNo, oldA.imports) || anyIn(addedNo, newA.imports)) &&
		slices.Equal(sideEffects(removedNo, oldA), sideEffects(addedNo, newA)) {
		return Imports
	}
	if lang == "go" {
		return ""
	}
	// Compare the block and the context around it, so a change that makes
	// the parser read an unchanged neighbour differently is seen too.
	oLo, oHi := span(b.oldNo[b.ctxStart:b.ctxEnd])
	nLo, nHi := span(b.newNo[b.ctxStart:b.ctxEnd])
	if hasLines(oldA.errors, oLo, oHi) || hasLines(newA.errors, nLo, nHi) {
		return ""
	}
	if sameTokens(oldA.tokens, oLo, oHi, newA.tokens, nLo, nHi) {
		return Format
	}
	return ""
}

// sameImports reports whether the import statements that the hunk's
// import-only blocks touch are the same on both sides once formatting and
// order are ignored, so the blocks only reorder or re-wrap imports.
func sameImports(h *patch.Hunk, blocks []block, oldA, newA *analysis) bool {
	oldStmts, newStmts := map[int]bool{}, map[int]bool{}
	for _, b := range blocks {
		if b.rule != Imports {
			continue
		}
		for i := b.start; i < b.end; i++ {
			if h.Lines[i].Kind == '-' {
				if k, ok := oldA.importOf[b.oldNo[i]]; ok {
					oldStmts[k] = true
				}
			} else if k, ok := newA.importOf[b.newNo[i]]; ok {
				newStmts[k] = true
			}
		}
	}
	keys := func(stmts map[int]bool, a *analysis) []string {
		var out []string
		for k := range stmts {
			out = append(out, a.importKeys[k])
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(keys(oldStmts, oldA), keys(newStmts, newA))
}

// importOnly reports whether every line is blank or part of an import.
func importOnly(texts []string, nos []int, a *analysis) bool {
	for i, no := range nos {
		if a.errors[no] {
			return false
		}
		_, sideEffect := a.sideEffect[no]
		if !a.imports[no] && !sideEffect && strings.TrimSpace(texts[i]) != "" {
			return false
		}
	}
	return true
}

// sideEffects lists the imports that only run code and start on the given
// lines. Adding, removing or reordering them changes what the program does,
// so the import rule needs the same list on both sides.
func sideEffects(nos []int, a *analysis) []string {
	var out []string
	for _, no := range nos {
		if s := a.sideEffect[no]; s != "" {
			out = append(out, s)
		}
	}
	return out
}

func anyIn(nos []int, m map[int]bool) bool {
	for _, no := range nos {
		if m[no] {
			return true
		}
	}
	return false
}

// span returns the smallest and largest non-zero line number, or 0, -1.
func span(nos []int) (int, int) {
	lo, hi := 0, -1
	for _, n := range nos {
		if n == 0 {
			continue
		}
		if lo == 0 || n < lo {
			lo = n
		}
		if n > hi {
			hi = n
		}
	}
	return lo, hi
}

func hasLines(m map[int]bool, lo, hi int) bool {
	for i := lo; i <= hi; i++ {
		if m[i] {
			return true
		}
	}
	return false
}

func sameTokens(a []tok, aLo, aHi int, b []tok, bLo, bHi int) bool {
	x, y := within(a, aLo, aHi), within(b, bLo, bHi)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i].text != y[i].text {
			return false
		}
	}
	return true
}

// within returns the tokens that touch lines lo..hi.
func within(ts []tok, lo, hi int) []tok {
	var out []tok
	for _, t := range ts {
		if t.last >= lo && t.first <= hi {
			out = append(out, t)
		}
	}
	return out
}

// keptHunks rebuilds h from its kept blocks. Each run of kept blocks becomes
// one hunk with all the context around it; folded blocks are left out, so
// applying the result leaves their lines as they were. A hunk with nothing
// folded comes back unchanged.
func (s *skimmer) keptHunks(f *patch.File, h *patch.Hunk, blocks []block) []*patch.Hunk {
	allKept := true
	for _, b := range blocks {
		if b.rule != "" || b.lines != nil {
			allKept = false
		}
	}
	if allKept {
		c := *h
		return []*patch.Hunk{s.keep(&c, nil)}
	}
	var out []*patch.Hunk
	for i := 0; i < len(blocks); {
		if b := blocks[i]; b.rule != "" {
			s.fold(Fold{Path: f.Path(), Line: b.newLine, Rule: b.rule, Lines: b.end - b.start})
			i++
			continue
		}
		j := i
		for j+1 < len(blocks) && blocks[j+1].rule == "" {
			j++
		}
		var here []Fold
		for _, b := range blocks[i : j+1] {
			if b.wsLines > 0 {
				f := Fold{Path: f.Path(), Line: b.wsLine, Rule: Whitespace, Lines: b.wsLines}
				s.r.Folds = append(s.r.Folds, f)
				here = append(here, f)
			}
		}
		out = append(out, s.keep(subHunk(h, blocks[i:j+1]), here))
		i = j + 1
	}
	return out
}

// subHunk builds the hunk for a run of kept blocks and all the context
// around them.
func subHunk(h *patch.Hunk, blocks []block) *patch.Hunk {
	first, last := blocks[0], blocks[len(blocks)-1]
	lo, hi := first.ctxStart, last.ctxEnd
	s := &patch.Hunk{Heading: h.Heading}
	for i, k := lo, 0; i < hi; {
		if k < len(blocks) && i == blocks[k].start {
			b := blocks[k]
			if b.lines != nil {
				s.Lines = append(s.Lines, b.lines...)
			} else {
				s.Lines = append(s.Lines, h.Lines[b.start:b.end]...)
			}
			i = b.end
			k++
			continue
		}
		s.Lines = append(s.Lines, h.Lines[i])
		i++
	}
	for _, l := range s.Lines {
		if l.Kind != '+' {
			s.OldLines++
		}
		if l.Kind != '-' {
			s.NewLines++
		}
	}
	s.OldStart = firstNo(first.oldNo, lo, hi)
	s.NewStart = firstNo(first.newNo, lo, hi)
	if s.OldLines == 0 {
		s.OldStart = positionBefore(h, first.oldNo, lo, true)
	}
	if s.NewLines == 0 {
		s.NewStart = positionBefore(h, first.newNo, lo, false)
	}
	return s
}

func firstNo(nos []int, lo, hi int) int {
	for i := lo; i < hi; i++ {
		if nos[i] != 0 {
			return nos[i]
		}
	}
	return 0
}

// foldWhitespaceLines pairs removed and added lines that are equal once
// whitespace is collapsed (a longest common subsequence, as git diff pairs
// lines) and turns each pair into one context line with the old text.
// It returns nil when no pair is found.
func foldWhitespaceLines(ls []patch.Line, newNo []int) ([]patch.Line, int, int) {
	var rem, add []int // indices into ls
	for i, l := range ls {
		if l.Kind == '-' {
			rem = append(rem, i)
		} else {
			add = append(add, i)
		}
	}
	key := func(i int) string {
		if ls[i].NoEOL != "" {
			return "\x00" + strconv.Itoa(i) // never pair a line whose final newline changed
		}
		return collapseSpace(ls[i].Text)
	}
	n, m := len(rem), len(add)
	if n == 0 || m == 0 || n*m > 4_000_000 {
		return nil, 0, 0
	}
	// lcs[i][j] is the LCS length of rem[i:] and add[j:].
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if key(rem[i]) == key(add[j]) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	if lcs[0][0] == 0 {
		return nil, 0, 0
	}
	var out []patch.Line
	folded, firstLine := 0, 0
	i, j := 0, 0
	var pendRem, pendAdd []patch.Line
	flush := func() {
		out = append(append(out, pendRem...), pendAdd...)
		pendRem, pendAdd = nil, nil
	}
	for i < n || j < m {
		switch {
		case i < n && j < m && key(rem[i]) == key(add[j]) && lcs[i][j] == lcs[i+1][j+1]+1:
			flush()
			out = append(out, patch.Line{Kind: ' ', Text: ls[rem[i]].Text})
			if firstLine == 0 {
				firstLine = newNo[add[j]]
			}
			folded += 2
			i++
			j++
		case j >= m || (i < n && lcs[i+1][j] >= lcs[i][j+1]):
			pendRem = append(pendRem, ls[rem[i]])
			i++
		default:
			pendAdd = append(pendAdd, ls[add[j]])
			j++
		}
	}
	flush()
	return out, folded, firstLine
}

// positionBefore is the number of the last line on one side before index lo,
// which is how a hunk side with no lines names its position.
func positionBefore(h *patch.Hunk, nos []int, lo int, old bool) int {
	for i := lo - 1; i >= 0; i-- {
		if nos[i] != 0 {
			return nos[i]
		}
	}
	if old {
		return start(h.OldStart, h.OldLines) - 1
	}
	return start(h.NewStart, h.NewLines) - 1
}
