// Package wincap records a maintenance window's traffic (owner,
// 2026-11-18): the purpose is a firmware update installed in a window,
// recorded in full and kept. Every chunk of every connection is kept
// verbatim as received, per direction and before any parsing or policy,
// so nothing about the firmware's framing (never seen on this inverter)
// needs to be known. The layout is prox's capture layout, which mon150
// reads:
//
//	<root>/<window id>/window.json            the window's summary
//	<root>/<window id>/<conn id>/c2s.bin      inverter → cloud bytes
//	<root>/<window id>/<conn id>/s2c.bin      cloud → inverter bytes
//	<root>/<window id>/<conn id>/meta.jsonl   one event per write
//
// A window stops recording at maxBytes (a runaway transfer must not fill
// the disk). Captures are kept until deleted; nothing ages them out.
package wincap

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// metaType is the meta.jsonl event-type key.
const metaType = "type"

// idLayout names a window by its start (UTC, filesystem-safe).
const idLayout = "20060102T150405Z"

// validID is a window id (never a path).
var validID = regexp.MustCompile(`^\d{8}T\d{6}Z$`)

// ErrNotFound reports no such capture.
var ErrNotFound = errors.New("wincap: no such capture")

// ErrRecording reports a capture still recording (not deletable).
var ErrRecording = errors.New("wincap: the capture is still recording")

// File modes: the captures hold the cloud's traffic (serials included).
const (
	dirMode  = 0o700
	fileMode = 0o600
)

// Info is one window's summary (window.json; List's rows).
type Info struct {
	ID      string     `json:"id"`
	Started time.Time  `json:"started"`
	Stopped *time.Time `json:"stopped,omitempty"`
	Bytes   int64      `json:"bytes"`
	Conns   int        `json:"connections"`
	Capped  bool       `json:"capped"` // recording stopped at the size cap
	Active  bool       `json:"active"`
	Version string     `json:"version"`
}

// Capturer records the windows.
type Capturer struct {
	root     string
	maxBytes int64
	clock    func() time.Time
	version  string

	mu     sync.Mutex
	active *Info
	conns  map[string]bool
	offs   map[string]int64 // conn+dir → bytes written
}

// New builds a capturer storing under root.
func New(root string, maxBytes int64, clock func() time.Time, version string) *Capturer {
	return &Capturer{root: root, maxBytes: maxBytes, clock: clock, version: version}
}

// Start begins recording (a window opened with capture); while one is
// recording it returns that one (a re-arm).
func (c *Capturer) Start() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		return c.active.ID, nil
	}
	now := c.clock().UTC()
	info := &Info{ID: now.Format(idLayout), Started: now, Active: true, Version: c.version}
	if err := os.MkdirAll(filepath.Join(c.root, info.ID), dirMode); err != nil {
		return "", fmt.Errorf("wincap: %w", err)
	}
	c.active, c.conns, c.offs = info, map[string]bool{}, map[string]int64{}
	if err := c.saveLocked(); err != nil {
		return "", err
	}
	return info.ID, nil
}

// Stop ends recording (the window closed or expired).
func (c *Capturer) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return
	}
	now := c.clock().UTC()
	c.active.Stopped, c.active.Active = &now, false
	if err := c.saveLocked(); err != nil {
		slog.Error("wincap: summary not saved", "err", err)
	}
	c.active = nil
}

// Active reports whether a window is recording.
func (c *Capturer) Active() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active != nil
}

// Chunk records one chunk of a connection (s2c: cloud → inverter); a
// no-op outside a recording window.
func (c *Capturer) Chunk(connID string, s2c bool, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || len(data) == 0 || c.active.Capped {
		return
	}
	if c.active.Bytes+int64(len(data)) > c.maxBytes {
		c.active.Capped = true
		slog.Warn("wincap: the capture reached its size cap; recording stopped", "window", c.active.ID, "max_bytes", c.maxBytes)
		_ = c.saveLocked()
		return
	}
	if err := c.writeLocked(connID, s2c, data); err != nil {
		slog.Error("wincap: chunk not recorded", "err", err)
		return
	}
	c.active.Bytes += int64(len(data))
}

// writeLocked appends one chunk and its meta event (files opened per
// write: chunks are seconds apart, and nothing stays open).
func (c *Capturer) writeLocked(connID string, s2c bool, data []byte) error {
	if !validConn(connID) {
		return fmt.Errorf("wincap: connection id %q", connID)
	}
	dir := filepath.Join(c.root, c.active.ID, connID)
	if !c.conns[connID] {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return err
		}
		c.conns[connID] = true
		c.active.Conns = len(c.conns)
		if err := appendJSON(filepath.Join(dir, "meta.jsonl"), map[string]any{metaType: "meta", "ts": c.ms(), "commit": c.version}); err != nil {
			return err
		}
		if err := appendJSON(filepath.Join(dir, "meta.jsonl"), map[string]any{metaType: "conn_open", "ts": c.ms(), "id": connID, "kind": "tls"}); err != nil {
			return err
		}
	}
	name, kind := "c2s.bin", "bytes_c2s"
	if s2c {
		name, kind = "s2c.bin", "bytes_s2c"
	}
	if err := appendFile(filepath.Join(dir, name), data); err != nil {
		return err
	}
	key := connID + "/" + name
	start := c.offs[key]
	c.offs[key] = start + int64(len(data))
	return appendJSON(filepath.Join(dir, "meta.jsonl"), map[string]any{metaType: kind, "ts": c.ms(), "start": start, "end": c.offs[key]})
}

func (c *Capturer) ms() int64 { return c.clock().UnixMilli() }

// saveLocked writes the active window's summary.
func (c *Capturer) saveLocked() error {
	b, err := json.MarshalIndent(c.active, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.root, c.active.ID, "window.json"), b, fileMode)
}

// List is every capture, newest first.
func (c *Capturer) List() ([]Info, error) {
	entries, err := os.ReadDir(c.root)
	if errors.Is(err, fs.ErrNotExist) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wincap: %w", err)
	}
	out := []Info{}
	for _, e := range entries {
		if !e.IsDir() || !validID.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(c.root, e.Name(), "window.json"))
		if err != nil {
			continue
		}
		var info Info
		if json.Unmarshal(b, &info) != nil {
			continue
		}
		c.mu.Lock()
		if info.Active && (c.active == nil || c.active.ID != info.ID) {
			info.Active = false // a run that stopped mid-window
		}
		c.mu.Unlock()
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// Archive writes one capture as a .tar.gz (paths under its id).
func (c *Capturer) Archive(w io.Writer, id string) error {
	dir, err := c.dir(id)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(c.root, path)
		if err != nil {
			return err
		}
		return addFile(tw, path, filepath.ToSlash(rel))
	})
	if err != nil {
		return fmt.Errorf("wincap: archive: %w", err)
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// Delete removes one capture (not the recording one).
func (c *Capturer) Delete(id string) error {
	dir, err := c.dir(id)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil && c.active.ID == id {
		return ErrRecording
	}
	return os.RemoveAll(dir)
}

// dir is a capture's directory, for a well-formed existing id only.
func (c *Capturer) dir(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", ErrNotFound
	}
	dir := filepath.Join(c.root, id)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", ErrNotFound
	}
	return dir, nil
}

// validConn: a connection id is a path element (prox's "<ms>-<seq>").
var validConnID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validConn(id string) bool { return validConnID.MatchString(id) && id != "." && id != ".." }

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode) //nolint:gosec // G304: under the captures root, validated ids
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func appendJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return appendFile(path, append(b, '\n'))
}

func addFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path) //nolint:gosec // G304: walked under a validated capture directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: fileMode, Size: st.Size(), ModTime: st.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
