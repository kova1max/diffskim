package skim

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"slices"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// analysis is what the import and format rules need to know about one side
// of a file. Line numbers are 1-based.
type analysis struct {
	imports map[int]bool // lines of import statements
	// sideEffect maps the lines of imports that only run code ("import './x'",
	// Go "_" imports) to the imported path, on the statement's first line,
	// or to "" on its other lines.
	sideEffect map[int]string
	errors     map[int]bool // lines the parser could not make sense of
	// importOf maps every line of an import statement to its index in
	// importKeys, which hold each statement's source and bindings with
	// formatting and binding order normalized away.
	importOf   map[int]int
	importKeys []string
	tokens     []tok // nil when the format rule does not apply
}

// tok is one entry of a file's normalized token stream: a leaf token or the
// start or end of a syntax node, with the lines it spans.
type tok struct {
	text        string
	first, last int
}

// language picks the grammar for a path, or returns "" when no import or
// format rule covers it.
func language(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".go":
		return "go"
	}
	return ""
}

func analyze(lang string, src []byte) *analysis {
	if lang == "go" {
		return analyzeGo(src)
	}
	var l *sitter.Language
	switch lang {
	case "javascript":
		l = sitter.NewLanguage(javascript.Language())
	case "typescript":
		l = sitter.NewLanguage(typescript.LanguageTypescript())
	case "tsx":
		l = sitter.NewLanguage(typescript.LanguageTSX())
	}
	p := sitter.NewParser()
	defer p.Close()
	if err := p.SetLanguage(l); err != nil {
		return nil
	}
	tree := p.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()
	a := newAnalysis()
	a.walk(tree.RootNode(), src)
	return a
}

func newAnalysis() *analysis {
	return &analysis{imports: map[int]bool{}, sideEffect: map[int]string{}, errors: map[int]bool{}, importOf: map[int]int{}}
}

func (a *analysis) addImport(first, last int, key string) {
	for i := first; i <= last; i++ {
		a.importOf[i] = len(a.importKeys)
	}
	a.importKeys = append(a.importKeys, key)
}

// atomic node kinds are compared as a whole instead of child by child, so no
// text inside them (template literal text, JSX text) can go unseen.
var atomic = map[string]bool{
	"string": true, "template_string": true, "regex": true, "comment": true,
	"html_comment": true, "jsx_text": true, "hash_bang_line": true,
}

func lines(n *sitter.Node) (int, int) {
	s, e := n.StartPosition(), n.EndPosition()
	first, last := int(s.Row)+1, int(e.Row)+1
	if e.Column == 0 && e.Row > s.Row {
		last-- // the node ends with a newline
	}
	return first, last
}

func (a *analysis) markSideEffect(first, last int, source string) {
	for i := first; i <= last; i++ {
		a.sideEffect[i] = ""
	}
	a.sideEffect[first] = "import " + source
}

func mark(m map[int]bool, first, last int) {
	for i := first; i <= last; i++ {
		m[i] = true
	}
}

func (a *analysis) walk(n *sitter.Node, src []byte) {
	first, last := lines(n)
	if n.IsError() || n.IsMissing() {
		mark(a.errors, first, last)
	}
	kind := n.Kind()
	if kind == "import_statement" {
		if !hasChild(n, "import_clause") && !hasChild(n, "import_require_clause") {
			source := ""
			if s := n.ChildByFieldName("source"); s != nil {
				source = unquoteJS(s.Utf8Text(src))
			}
			a.markSideEffect(first, last, source)
		} else {
			mark(a.imports, first, last)
		}
		a.addImport(first, last, importKey(n, src))
	}
	if atomic[kind] {
		a.tokens = append(a.tokens, tok{atomicText(n, src), first, last})
		return
	}
	count := n.ChildCount()
	if count == 0 {
		text := n.Utf8Text(src)
		if n.IsNamed() {
			text = kind + "=" + text
		} else if text == ";" {
			return // semicolons are optional, and where ASI changes meaning the structure differs
		}
		a.tokens = append(a.tokens, tok{text, first, last})
		return
	}
	if n.IsNamed() {
		a.tokens = append(a.tokens, tok{"(" + kind, first, first})
	}
	for i := uint(0); i < count; i++ {
		c := n.Child(i)
		if c.Kind() == "," && !c.IsNamed() && i+1 < count && closesList(n.Child(i+1)) {
			continue // trailing comma
		}
		if f := n.FieldNameForChild(uint32(i)); f != "" {
			cf, _ := lines(c)
			a.tokens = append(a.tokens, tok{f + ":", cf, cf})
		}
		a.walk(c, src)
	}
	if n.IsNamed() {
		a.tokens = append(a.tokens, tok{")", last, last})
	}
}

// importKey is the text of an import statement without whitespace,
// semicolons or commas, with its named bindings sorted.
func importKey(n *sitter.Node, src []byte) string {
	if atomic[n.Kind()] {
		return atomicText(n, src)
	}
	if n.ChildCount() == 0 {
		if t := n.Utf8Text(src); t != ";" && t != "," {
			return t
		}
		return ""
	}
	var parts []string
	for i := uint(0); i < n.ChildCount(); i++ {
		if k := importKey(n.Child(i), src); k != "" {
			parts = append(parts, k)
		}
	}
	if n.Kind() == "named_imports" {
		slices.Sort(parts)
	}
	return "(" + strings.Join(parts, " ") + ")"
}

func hasChild(n *sitter.Node, kind string) bool {
	for i := uint(0); i < n.ChildCount(); i++ {
		if n.Child(i).Kind() == kind {
			return true
		}
	}
	return false
}

func closesList(n *sitter.Node) bool {
	if n.IsNamed() {
		return false
	}
	switch n.Kind() {
	case ")", "]", "}", ">":
		return true
	}
	return false
}

func atomicText(n *sitter.Node, src []byte) string {
	text := n.Utf8Text(src)
	switch n.Kind() {
	case "string":
		if p := n.Parent(); p != nil && p.Kind() == "jsx_attribute" {
			return "string=" + text // JSX attribute strings have no escapes
		}
		return "string=" + unquoteJS(text)
	case "comment", "html_comment":
		return "comment=" + strings.Join(strings.Fields(text), " ")
	case "jsx_text":
		return "jsx_text=" + jsxText(text)
	}
	return n.Kind() + "=" + text
}

// unquoteJS drops the quotes of a string literal and the escapes of quote
// characters, so 'it\'s' and "it's" compare equal. Every other escape stays
// as written.
func unquoteJS(s string) string {
	if len(s) < 2 {
		return s
	}
	s = s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			if s[i+1] == '\'' || s[i+1] == '"' {
				b.WriteByte(s[i+1])
			} else {
				b.WriteString(s[i : i+2])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// jsxText applies the JSX whitespace rule: lines are trimmed, blank lines are
// dropped and the rest are joined with one space. Text that only changed in
// a way this rule erases renders the same.
func jsxText(s string) string {
	ls := strings.Split(s, "\n")
	var out []string
	for i, l := range ls {
		if i > 0 {
			l = strings.TrimLeft(l, " \t\r")
		}
		if i < len(ls)-1 {
			l = strings.TrimRight(l, " \t\r")
		}
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, " ")
}

// analyzeGo finds Go import lines. Go code is gofmt'ed almost everywhere, so
// the format rule does not cover it and tokens stay nil.
func analyzeGo(src []byte) *analysis {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	a := newAnalysis()
	line := func(p token.Pos) int { return fset.Position(p).Line }
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.IMPORT {
			continue
		}
		a.imports[line(g.TokPos)] = true
		if g.Lparen.IsValid() {
			a.imports[line(g.Lparen)] = true
			a.imports[line(g.Rparen)] = true
		}
		for _, s := range g.Specs {
			spec := s.(*ast.ImportSpec)
			first, last := line(spec.Pos()), line(spec.End())
			key := spec.Path.Value
			if spec.Name != nil {
				key = spec.Name.Name + " " + key
			}
			if spec.Name != nil && (spec.Name.Name == "_" || spec.Name.Name == ".") {
				a.markSideEffect(first, last, key)
			} else {
				mark(a.imports, first, last)
			}
			a.addImport(first, last, key)
		}
	}
	return a
}
