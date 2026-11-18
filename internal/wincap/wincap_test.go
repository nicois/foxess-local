package wincap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newCap(t *testing.T, maxBytes int64) (*Capturer, string) {
	t.Helper()
	root := t.TempDir()
	clock := time.Date(2026, 11, 18, 1, 35, 0, 0, time.UTC)
	return New(root, maxBytes, func() time.Time { clock = clock.Add(time.Second); return clock }, "vtest"), root
}

// TestCaptureWindow (owner, 2026-11-18): while a window captures, every
// chunk of every connection is kept verbatim, per direction, in a prox
// capture layout (mon150 reads it); outside a window nothing is kept;
// the window's summary says what it holds.
func TestCaptureWindow(t *testing.T) {
	c, root := newCap(t, 1<<20)
	c.Chunk("c1", true, []byte("before")) // no window: not kept
	id, err := c.Start()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := c.Start(); again != id {
		t.Fatalf("a re-arm keeps the window: %s vs %s", again, id)
	}
	c.Chunk("c1", false, []byte{0x7f, 0x7f, 1})
	c.Chunk("c1", true, []byte{0xa5, 0xa5})
	c.Chunk("c1", true, []byte{0xa6})
	c.Chunk("c2", true, []byte("second connection"))
	c.Stop()
	c.Chunk("c1", true, []byte("after")) // closed: not kept

	s2c, err := os.ReadFile(filepath.Join(root, id, "c1", "s2c.bin")) //nolint:gosec // G304: the test's temp dir
	if err != nil || !bytes.Equal(s2c, []byte{0xa5, 0xa5, 0xa6}) {
		t.Fatalf("s2c %x %v", s2c, err)
	}
	c2s, _ := os.ReadFile(filepath.Join(root, id, "c1", "c2s.bin")) //nolint:gosec // G304: the test's temp dir
	if !bytes.Equal(c2s, []byte{0x7f, 0x7f, 1}) {
		t.Fatalf("c2s %x", c2s)
	}
	meta, _ := os.ReadFile(filepath.Join(root, id, "c1", "meta.jsonl")) //nolint:gosec // G304: the test's temp dir
	if !strings.Contains(string(meta), `"type":"bytes_s2c"`) || !strings.Contains(string(meta), `"type":"meta"`) {
		t.Fatalf("meta %s", meta)
	}
	list, err := c.List()
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Bytes != 3+3+17 || list[0].Conns != 2 || list[0].Active || list[0].Capped {
		t.Fatalf("list %+v %v", list, err)
	}
}

// TestCaptureCap: a window stops recording at the cap (a runaway
// transfer cannot fill the disk) and says so.
func TestCaptureCap(t *testing.T) {
	c, _ := newCap(t, 10)
	id, _ := c.Start()
	c.Chunk("c1", true, make([]byte, 8))
	c.Chunk("c1", true, make([]byte, 8)) // over the cap: not kept
	list, _ := c.List()
	if list[0].ID != id || list[0].Bytes != 8 || !list[0].Capped || !list[0].Active {
		t.Fatalf("capped %+v", list)
	}
}

// TestCaptureArchiveDelete: a window downloads as one .tar.gz of its
// files and is deleted on request; a malformed id is refused (no path
// escapes the captures directory).
func TestCaptureArchiveDelete(t *testing.T) {
	c, root := newCap(t, 1<<20)
	id, _ := c.Start()
	c.Chunk("c1", true, []byte{1, 2, 3})
	c.Stop()
	var buf bytes.Buffer
	if err := c.Archive(&buf, id); err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	found := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		found[h.Name] = b
	}
	if !bytes.Equal(found[id+"/c1/s2c.bin"], []byte{1, 2, 3}) || found[id+"/window.json"] == nil {
		t.Fatalf("archive %v", keys(found))
	}
	for _, bad := range []string{"../etc", "x/../../y", ""} {
		if err := c.Delete(bad); err == nil {
			t.Fatalf("delete %q accepted", bad)
		}
	}
	if err := c.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
		t.Fatalf("not deleted: %v", err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCaptureStaleActive: a capture left "active" by a run that stopped
// mid-window (the window does not survive a restart) lists as stopped
// and can be deleted.
func TestCaptureStaleActive(t *testing.T) {
	c, root := newCap(t, 1<<20)
	id, _ := c.Start()
	c2 := New(root, 1<<20, time.Now, "vtest") // the next run
	list, _ := c2.List()
	if len(list) != 1 || list[0].Active {
		t.Fatalf("stale active: %+v", list)
	}
	if err := c2.Delete(id); err != nil {
		t.Fatal(err)
	}
}
