package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestScriptsParse (2026-11-19): every UI script parses as an ES module.
// A stray parenthesis in app.js left the whole UI blank, and
// `node --check file.js` did not see it (Node 22 does not check a .js
// file as a module); each script is checked as .mjs instead.
func TestScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the UI scripts' syntax is not checked here")
	}
	scripts, _ := filepath.Glob("static/js/*.js")
	if len(scripts) == 0 {
		t.Fatal("no scripts found")
	}
	dir := t.TempDir()
	for _, f := range scripts {
		src, err := os.ReadFile(f) //nolint:gosec // G304: the package's own static files
		if err != nil {
			t.Fatal(err)
		}
		mjs := filepath.Join(dir, filepath.Base(f)+".mjs")
		if err := os.WriteFile(mjs, src, 0o600); err != nil { //nolint:gosec // G703: a test temp dir
			t.Fatal(err)
		}
		//nolint:gosec // G204: node on a copy of this package's script
		if out, err := exec.Command(node, "--check", mjs).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse as a module:\n%s", f, out)
		}
	}
}

// TestSocView runs the dashboard SoC tests (testdata/socview.test.mjs)
// under node's test runner.
func TestSocView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the SoC display tests do not run here")
	}
	//nolint:gosec // G204: node on this package's own test file
	if out, err := exec.Command(node, "--test", "testdata/socview.test.mjs").CombinedOutput(); err != nil {
		t.Fatalf("socview tests:\n%s", out)
	}
}

// TestPeriods runs the diagnostics period tests (testdata/periods.test.mjs)
// under node's test runner.
func TestPeriods(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the period choice tests do not run here")
	}
	//nolint:gosec // G204: node on this package's own test file
	if out, err := exec.Command(node, "--test", "testdata/periods.test.mjs").CombinedOutput(); err != nil {
		t.Fatalf("periods tests:\n%s", out)
	}
}
