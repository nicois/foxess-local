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
