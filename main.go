// Command diffskim reads a unified diff and writes it back without the hunks
// that are only formatting, import shuffles, lockfiles or generated code.
//
//	git diff main... | diffskim | delta
//	diffskim main...
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kova1max/diffskim/internal/patch"
	"github.com/kova1max/diffskim/internal/skim"
)

const usage = `usage: git diff [<args>] | diffskim [options]
       diffskim [options] [<git diff args>]

Reads a unified diff on stdin, or runs git diff with the given arguments,
and prints it without the hunks that are only lockfile, generated-code,
whitespace, import or formatting changes. Output is still a patch that
git apply accepts. A summary goes to stderr.

options:
  --explain   print one line per fold instead of the diff
  --json      print the summary and every fold as JSON instead of the diff
  -q, --quiet no summary on stderr
  --lockfiles print the lockfile names that are folded (and excluded from
              the git diff -M -w baseline in the summary)
  -h, --help  show this help
  --version   print the version

exit status: 0 on success, 1 on error, 2 on bad usage
`

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type options struct {
	explain, json, quiet bool
	gitArgs              []string
}

func parseArgs(args []string) (options, error) {
	var o options
	for i, a := range args {
		switch a {
		case "--explain":
			o.explain = true
		case "--json":
			o.json = true
		case "-q", "--quiet":
			o.quiet = true
		case "--":
			o.gitArgs = append(o.gitArgs, args[i:]...)
			return o, o.check()
		default:
			o.gitArgs = append(o.gitArgs, a)
		}
	}
	return o, o.check()
}

func (o options) check() error {
	if o.explain && o.json {
		return errors.New("--explain and --json cannot be used together")
	}
	return nil
}

func run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "--" {
			break
		}
		switch a {
		case "-h", "--help":
			fmt.Fprint(stdout, usage)
			return 0
		case "--version":
			fmt.Fprintln(stdout, "diffskim", version)
			return 0
		case "--lockfiles":
			fmt.Fprintln(stdout, strings.Join(skim.LockfileNames, "\n"))
			return 0
		}
	}
	o, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "diffskim: %v\n%s", err, usage)
		return 2
	}
	if err := skimAndPrint(o, stdin, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "diffskim: %v\n", err)
		return 1
	}
	return 0
}

func skimAndPrint(o options, stdin *os.File, stdout, stderr io.Writer) error {
	repo := openRepo()
	defer repo.close()

	// With arguments, or with nothing piped in, diffskim runs git diff itself.
	useGit := len(o.gitArgs) > 0 || isTerminal(stdin)
	var input []byte
	var err error
	if useGit {
		if repo == nil {
			return errors.New("not in a git repository (pipe a diff in instead)")
		}
		input, err = gitOutput("", append(append(append([]string{}, gitDiffArgs...), "-M", "--full-index"), o.gitArgs...)...)
	} else {
		input, err = io.ReadAll(stdin)
	}
	if err != nil {
		return err
	}
	p, err := patch.Parse(string(input))
	if err != nil {
		return fmt.Errorf("cannot parse the diff: %v", err)
	}

	var src skim.Source
	if repo != nil {
		repo.loadAttributes(p)
		src = repo
	}
	r := skim.Skim(p, src)

	baseline := -1
	if useGit {
		baseline = baselineChanged(o.gitArgs)
	}

	switch {
	case o.json:
		if err := writeJSON(stdout, r, baseline); err != nil {
			return err
		}
	case o.explain:
		for _, f := range r.Folds {
			fmt.Fprintln(stdout, explainLine(f))
		}
	default:
		if err := r.Patch.Write(stdout); err != nil {
			return err
		}
	}
	if !o.quiet && !o.json {
		fmt.Fprintln(stderr, summary(r, baseline))
	}
	return nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func lockfileExcludes() []string {
	var out []string
	for _, n := range skim.LockfileNames {
		out = append(out, ":(exclude,glob)**/"+n)
	}
	return out
}

func explainLine(f skim.Fold) string {
	where := f.Path
	if f.Line > 0 {
		where = fmt.Sprintf("%s:%d", f.Path, f.Line)
	}
	s := fmt.Sprintf("%s %s (%d %s)", where, f.Rule, f.Lines, plural(f.Lines, "line"))
	if f.Reason != "" {
		s += ": " + f.Reason
	}
	return s
}

// summary is the stderr line, for example:
//
//	diffskim: 2140 changed lines -> 140 kept (git diff -M -w + lockfile excludes: 400); folded lockfile 1800, format-only 200
func summary(r skim.Result, baseline int) string {
	if r.Changed == 0 {
		return "diffskim: no changed lines"
	}
	s := fmt.Sprintf("diffskim: %d changed %s -> %d kept", r.Changed, plural(r.Changed, "line"), r.Kept)
	if baseline >= 0 {
		s += fmt.Sprintf(" (git diff -M -w + lockfile excludes: %d)", baseline)
	}
	var parts []string
	for _, rule := range skim.Rules {
		if n := byRule(r.Folds)[rule]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", rule, n))
		}
	}
	if len(parts) > 0 {
		s += "; folded " + strings.Join(parts, ", ")
	}
	if r.Unchecked > 0 {
		s += fmt.Sprintf("; import/format rules skipped %d %s (file contents not in this repository)", r.Unchecked, plural(r.Unchecked, "line"))
	}
	return s
}

func byRule(folds []skim.Fold) map[skim.Rule]int {
	m := map[skim.Rule]int{}
	for _, f := range folds {
		m[f.Rule] += f.Lines
	}
	return m
}

func writeJSON(w io.Writer, r skim.Result, baseline int) error {
	type report struct {
		Changed   int               `json:"changed"`
		Kept      int               `json:"kept"`
		Baseline  *int              `json:"baseline"`
		Folded    map[skim.Rule]int `json:"folded"`
		Unchecked int               `json:"unchecked"`
		Folds     []skim.Fold       `json:"folds"`
	}
	rep := report{Changed: r.Changed, Kept: r.Kept, Folded: byRule(r.Folds), Unchecked: r.Unchecked, Folds: r.Folds}
	if baseline >= 0 {
		rep.Baseline = &baseline
	}
	if rep.Folds == nil {
		rep.Folds = []skim.Fold{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
