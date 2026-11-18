// Command changelog reads CHANGELOG.md (Keep a Changelog form) for the
// release tooling (owner, 2026-11-18):
//
//	changelog next -from v0.1.1 [-bump minor]  # the next version, from the Unreleased entries
//	changelog release -version v0.1.2 [-date YYYY-MM-DD]  # Unreleased becomes that version
//	changelog notes -version v0.1.2            # that version's section (the release notes)
//
// The bump: Breaking or Removed → major (minor before 1.0); Added →
// minor; Changed, Deprecated, Fixed, Security → patch. -bump may raise
// it, never lower it. An empty Unreleased section is an error: every
// release says what is in it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	file      = "CHANGELOG.md"
	fileMode  = 0o644 // a tracked text file
	minArgs   = 2     // the command name and a subcommand
	exitUsage = 2
)

// The bump levels, smallest first.
const (
	patch = iota
	minor
	major
)

var levelNames = map[string]int{"patch": patch, "minor": minor, "major": major}

// categories and the bump each implies.
var categories = map[string]int{
	"Breaking": major, "Removed": major,
	"Added":   minor,
	"Changed": patch, "Deprecated": patch, "Fixed": patch, "Security": patch,
}

var (
	versionRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	sectionRe = regexp.MustCompile(`^## \[([^\]]+)\]`)
	catRe     = regexp.MustCompile(`^### (.+?)\s*$`)
)

const unreleasedName = "Unreleased"

// section is one "## [name]" block: its categories in order, each with
// its entry count.
type section struct {
	name    string
	cats    []string
	entries map[string]int
}

type changelog struct{ sections []section }

// parse checks the structure: an [Unreleased] section, known categories,
// and no category without entries.
func parse(text string) (changelog, error) {
	var p parser
	for i, line := range strings.Split(text, "\n") {
		if err := p.line(i+1, line); err != nil {
			return p.cl, err
		}
	}
	if err := p.closeCat(); err != nil {
		return p.cl, err
	}
	if len(p.cl.sections) == 0 || p.cl.sections[0].name != unreleasedName {
		return p.cl, fmt.Errorf("%s: the first section must be ## [%s]", file, unreleasedName)
	}
	return p.cl, nil
}

// parser reads the changelog a line at a time.
type parser struct {
	cl  changelog
	cur *section // nil before the first section (the introduction)
	cat string   // the open category
}

func (p *parser) line(n int, line string) error {
	if m := sectionRe.FindStringSubmatch(line); m != nil {
		if err := p.closeCat(); err != nil {
			return err
		}
		p.cl.sections = append(p.cl.sections, section{name: m[1], entries: map[string]int{}})
		p.cur, p.cat = &p.cl.sections[len(p.cl.sections)-1], ""
		return nil
	}
	if p.cur == nil {
		return nil
	}
	if m := catRe.FindStringSubmatch(line); m != nil {
		if err := p.closeCat(); err != nil {
			return err
		}
		if _, ok := categories[m[1]]; !ok {
			return fmt.Errorf("%s:%d: unknown category %q (one of Breaking, Added, Changed, Deprecated, Removed, Fixed, Security)", file, n, m[1])
		}
		p.cat = m[1]
		p.cur.cats = append(p.cur.cats, p.cat)
		return nil
	}
	if p.cat != "" && strings.HasPrefix(line, "- ") {
		p.cur.entries[p.cat]++
	}
	return nil
}

// closeCat: a category must have entries.
func (p *parser) closeCat() error {
	if p.cur != nil && p.cat != "" && p.cur.entries[p.cat] == 0 {
		return fmt.Errorf("%s: [%s] ### %s has no entries", file, p.cur.name, p.cat)
	}
	return nil
}

// unreleased lists the Unreleased section's categories.
func (cl changelog) unreleased() []string { return cl.sections[0].cats }

// has reports whether a section for name exists.
func (cl changelog) has(name string) bool {
	return slices.ContainsFunc(cl.sections, func(s section) bool { return s.name == name })
}

// next is the version after from for entries in cats; override ("",
// minor, major, patch) may raise the bump.
func next(from string, cats []string, override string) (string, error) {
	if len(cats) == 0 {
		return "", errors.New("nothing under [Unreleased]: add entries to CHANGELOG.md first")
	}
	if from == "" {
		from = "v0.0.0"
	}
	m := versionRe.FindStringSubmatch(from)
	if m == nil {
		return "", fmt.Errorf("%q is not a version like v1.2.3", from)
	}
	v := [3]int{}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	level := patch
	for _, c := range cats {
		level = max(level, categories[c])
	}
	if override != "" {
		o, ok := levelNames[override]
		if !ok {
			return "", fmt.Errorf("bump %q: one of patch, minor, major", override)
		}
		if o < level {
			return "", fmt.Errorf("bump %s is below what the entries need (%s)", override, nameOf(level))
		}
		level = o
	}
	if level == major && v[0] == 0 {
		level = minor // 0.x: a breaking change bumps the minor version
	}
	switch level {
	case major:
		v = [3]int{v[0] + 1, 0, 0}
	case minor:
		v = [3]int{v[0], v[1] + 1, 0}
	default:
		v[2]++
	}
	return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]), nil
}

func nameOf(level int) string {
	for n, l := range levelNames {
		if l == level {
			return n
		}
	}
	return "?"
}

// release turns the Unreleased entries into version's section, dated,
// under a fresh empty [Unreleased].
func release(text, version, date string) (string, error) {
	cl, err := parse(text)
	if err != nil {
		return "", err
	}
	if len(cl.unreleased()) == 0 {
		return "", errors.New("nothing under [Unreleased]")
	}
	if cl.has(version) {
		return "", fmt.Errorf("%s already has a section", version)
	}
	head := "## [" + unreleasedName + "]"
	i := strings.Index(text, head)
	end := i + len(head)
	// the new heading goes right under [Unreleased]: the entries that
	// followed it are now the version's
	return text[:end] + "\n\n## [" + version + "] - " + date + text[end:], nil
}

// notesFor is version's section body (without its heading), trimmed.
func notesFor(text, version string) (string, error) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		m := sectionRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if start >= 0 {
			return strings.TrimSpace(strings.Join(lines[start:i], "\n")), nil
		}
		if m[1] == version {
			start = i + 1
		}
	}
	if start < 0 {
		return "", fmt.Errorf("%s has no section for %s", file, version)
	}
	return strings.TrimSpace(strings.Join(lines[start:], "\n")), nil
}

func main() {
	if len(os.Args) < minArgs {
		fmt.Fprintln(os.Stderr, "usage: changelog next|release|notes [flags]")
		os.Exit(exitUsage)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	from := fs.String("from", "", "next: the current version (the latest tag)")
	bump := fs.String("bump", "", "next: raise the bump to minor or major")
	version := fs.String("version", "", "release, notes: the version")
	date := fs.String("date", time.Now().UTC().Format(time.DateOnly), "release: the release date")
	path := fs.String("file", file, "the changelog")
	_ = fs.Parse(os.Args[2:])
	if err := run(os.Args[1], *path, *from, *bump, *version, *date); err != nil {
		fmt.Fprintln(os.Stderr, "changelog:", err)
		os.Exit(1)
	}
}

func run(cmd, path, from, bump, version, date string) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the changelog the caller names
	if err != nil {
		return err
	}
	text := string(b)
	switch cmd {
	case "next":
		cl, err := parse(text)
		if err != nil {
			return err
		}
		v, err := next(from, cl.unreleased(), bump)
		if err != nil {
			return err
		}
		fmt.Println(v)
	case "release":
		if !versionRe.MatchString(version) {
			return fmt.Errorf("-version %q is not like v1.2.3", version)
		}
		out, err := release(text, version, date)
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte(out), fileMode) //nolint:gosec // G703: the changelog the operator names (-file)
	case "notes":
		n, err := notesFor(text, version)
		if err != nil {
			return err
		}
		fmt.Println(n)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}
