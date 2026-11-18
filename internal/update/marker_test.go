package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoteExit (install review G4): the app records an environmental
// exit cause on a pending upgrade's marker for the guard; with no
// pending upgrade it writes nothing.
func TestNoteExit(t *testing.T) {
	dir := t.TempDir()
	if err := NoteExit(dir, ExitPortInUse); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, PendingFile)); err == nil {
		t.Fatal("wrote a marker with no upgrade pending")
	}
	if err := WriteMarker(dir, Marker{From: "a", To: "b", Attempts: 1, Online: true}); err != nil {
		t.Fatal(err)
	}
	if err := NoteExit(dir, ExitPortInUse); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, PendingFile)) //nolint:gosec // G304: test temp dir
	if !strings.Contains(string(b), "exit=port_in_use") || !strings.Contains(string(b), "online=1") {
		t.Fatalf("marker %q", b)
	}
}
