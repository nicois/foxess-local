package update

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Marker files in the binary's directory, shared with the root-owned
// upgrade guard (deploy/upgrade-guard.sh, ExecStartPre): plain key=value
// lines so a POSIX shell can read them.
const (
	PendingFile = "upgrade-pending" // written by Apply; the guard counts start attempts
	FailedFile  = "upgrade-failed"  // written by the guard after it restored .prev
	markerMode  = 0o644
)

// Marker records an upgrade in flight (or one the guard rolled back).
type Marker struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Attempts int    `json:"attempts"`
	// Online: the inverter was online when the upgrade was applied, so
	// the new binary must bring it back before it is trusted (Service.Run)
	Online bool `json:"online_before"`
}

// WriteMarker records a pending upgrade.
func WriteMarker(dir string, m Marker) error {
	body := fmt.Sprintf("from=%s\nto=%s\nattempts=%d\n", m.From, m.To, m.Attempts)
	if m.Online {
		body += "online=1\n"
	}
	return os.WriteFile(filepath.Join(dir, PendingFile), []byte(body), markerMode)
}

func readMarkerFile(path string) (Marker, bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // a fixed name in the app's own directory
	if errors.Is(err, fs.ErrNotExist) {
		return Marker{}, false, nil
	}
	if err != nil {
		return Marker{}, false, err
	}
	var m Marker
	for l := range strings.SplitSeq(string(b), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "from":
			m.From = v
		case "to":
			m.To = v
		case "attempts":
			_, _ = fmt.Sscanf(v, "%d", &m.Attempts)
		case "online":
			m.Online = v == "1"
		}
	}
	return m, true, nil
}

// ExitPortInUse is the exit cause recorded when the inverter port was
// still held by a previous session after the bind wait (DEF-009): the
// environment, not the binary, so the guard does not count that start.
const ExitPortInUse = "port_in_use"

// NoteExit records why this start is ending on a pending upgrade's marker
// (install review G4), for the guard's next run. No pending upgrade: a
// no-op. The guard caps how many starts it leaves uncounted.
func NoteExit(dir, cause string) error {
	path := filepath.Join(dir, PendingFile)
	b, err := os.ReadFile(path) //nolint:gosec // a fixed name in the app's own directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	body := strings.TrimRight(string(b), "\n") + "\nexit=" + cause + "\n"
	return os.WriteFile(path, []byte(body), markerMode) //nolint:gosec // G703: a fixed name in the app's own directory
}

// ReadMarker returns the pending upgrade, if any.
func ReadMarker(dir string) (Marker, bool, error) {
	return readMarkerFile(filepath.Join(dir, PendingFile))
}

// ReadFailed returns the upgrade the guard rolled back, if any.
func ReadFailed(dir string) (Marker, bool, error) {
	return readMarkerFile(filepath.Join(dir, FailedFile))
}

// ConfirmHealthy ends a pending upgrade: the new binary has run long
// enough to be trusted, so the guard stops counting.
func ConfirmHealthy(dir string) error {
	err := os.Remove(filepath.Join(dir, PendingFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ClearFailed dismisses a rollback notice.
func ClearFailed(dir string) error {
	err := os.Remove(filepath.Join(dir, FailedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
