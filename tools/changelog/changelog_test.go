package main

import (
	"os"
	"strings"
	"testing"
)

const sample = `# Changelog

Intro text.

## [Unreleased]

### Fixed
- a fix

## [v0.1.1] - 2026-11-18

### Fixed
- key IDs read as minisign prints them

## [v0.1.0] - 2026-11-18

### Added
- first public release
`

const (
	fixed, added     = "Fixed", "Added"
	v011, v020, v142 = "v0.1.1", "v0.2.0", "v1.4.2"
)

// TestNext: the bump follows the Unreleased categories; pre-1.0 a
// breaking change bumps the minor version.
func TestNext(t *testing.T) {
	for _, c := range []struct {
		from, cats, bump, want string
	}{
		{v011, fixed, "", "v0.1.2"},
		{v011, "Changed Security", "", "v0.1.2"},
		{v011, "Added Fixed", "", v020},
		{v011, "Breaking", "", v020},
		{v011, "Removed", "", v020},
		{v142, "Breaking", "", "v2.0.0"},
		{v142, "Added", "", "v1.5.0"},
		{v142, fixed, "minor", "v1.5.0"},
		{v142, fixed, "major", "v2.0.0"},
		{"", "Added", "", "v0.1.0"},
	} {
		got, err := next(c.from, strings.Fields(c.cats), c.bump)
		if err != nil || got != c.want {
			t.Errorf("next(%s, %s, %q) = %s, %v; want %s", c.from, c.cats, c.bump, got, err, c.want)
		}
	}
	if _, err := next(v142, []string{added}, "patch"); err == nil {
		t.Error("an override below the entries' bump was accepted")
	}
	if _, err := next(v142, nil, ""); err == nil {
		t.Error("an empty Unreleased section was accepted")
	}
	if _, err := next("1.4.2", []string{fixed}, ""); err == nil {
		t.Error("a malformed version was accepted")
	}
}

// TestParse: sections, categories and entries; an unknown category or a
// category with no entries is an error.
func TestParse(t *testing.T) {
	cl, err := parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if got := cl.unreleased(); len(got) != 1 || got[0] != fixed {
		t.Fatalf("unreleased categories %v", got)
	}
	if _, err := parse(strings.Replace(sample, "### Fixed\n- a fix", "### Improved\n- a fix", 1)); err == nil {
		t.Error("unknown category accepted")
	}
	if _, err := parse(strings.Replace(sample, "### Fixed\n- a fix", "### Fixed\n", 1)); err == nil {
		t.Error("an empty category accepted")
	}
	if _, err := parse(strings.Replace(sample, "## [Unreleased]", "## Unreleased", 1)); err == nil {
		t.Error("a changelog without [Unreleased] accepted")
	}
}

// TestRelease: the Unreleased entries become the new version's section,
// under a fresh empty Unreleased; the notes are that section.
func TestRelease(t *testing.T) {
	out, err := release(sample, "v0.1.2", "2026-11-19")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "## [Unreleased]\n\n## [v0.1.2] - 2026-11-19\n\n### Fixed\n- a fix\n\n## [v0.1.1]") {
		t.Fatalf("released:\n%s", out)
	}
	cl, err := parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(cl.unreleased()) != 0 {
		t.Fatal("Unreleased not emptied")
	}
	notes, err := notesFor(out, "v0.1.2")
	if err != nil || notes != "### Fixed\n- a fix" {
		t.Fatalf("notes %q %v", notes, err)
	}
	if _, err := release(out, "v0.1.3", "2026-11-19"); err == nil {
		t.Error("released with nothing unreleased")
	}
	if _, err := release(sample, "v0.1.1", "2026-11-19"); err == nil {
		t.Error("released a version that already has a section")
	}
	if _, err := notesFor(sample, "v9.9.9"); err == nil {
		t.Error("notes for a missing version")
	}
}

// TestRepoChangelog: the repository's own CHANGELOG.md parses, so make
// tag and the release workflow can rely on it.
func TestRepoChangelog(t *testing.T) {
	b, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parse(string(b)); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v0.1.0", "v0.1.1"} {
		if n, err := notesFor(string(b), v); err != nil || n == "" {
			t.Errorf("%s: %q %v", v, n, err)
		}
	}
}
