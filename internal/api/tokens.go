package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TokenStore sentinels (the API maps them to the spec's problem codes).
var (
	errTokenNotFound = errors.New("tokens: not found")
	errUnknownRole   = errors.New("tokens: unknown role")
	errTokenName     = errors.New("tokens: invalid or duplicate name")
)

// Role is one token role (the spec hierarchy, lowest first).
type Role string

// Token roles (the spec's hierarchy, lowest first: read-only <
// standard < expert < installer).
const (
	RoleReadOnly  Role = "read-only"
	RoleStandard  Role = "standard"
	RoleExpert    Role = "expert"
	RoleInstaller Role = "installer"
)

// Role ranks (the auth check: the token's rank must be at least the
// endpoint's required rank).
const (
	rankReadOnly  = 1
	rankStandard  = 2
	rankExpert    = 3
	rankInstaller = 4
)

// roleLevel ranks the roles (the auth check: the token's level must be
// at least the endpoint's required level).
var roleLevel = map[Role]int{
	RoleReadOnly:  rankReadOnly,
	RoleStandard:  rankStandard,
	RoleExpert:    rankExpert,
	RoleInstaller: rankInstaller,
}

// Token is one named credential (the backing file holds the hash
// only — the plaintext exists exactly once, on create).
type Token struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Role     Role       `json:"role"`
	Hash     string     `json:"hash"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used"`
}

// TokenStore is the named-token registry. Backing: a JSON file at path
// (atomic tmp+rename rewrite; a missing file starts empty), or
// in-memory when path is empty.
type TokenStore struct {
	mu     sync.Mutex
	path   string
	clock  func() time.Time
	tokens map[string]Token
	seq    int
	// usedDirty: a lookup changed last_used since the last write
	usedDirty bool
}

// tokenFile is the backing file format (version 1).
type tokenFile struct {
	Version int     `json:"version"`
	Tokens  []Token `json:"tokens"`
}

// tokenFileVersion is the only file version the store understands.
const tokenFileVersion = 1

// tokenValuePrefix marks one minted value (the spec fl_ prefix).
const tokenValuePrefix = "fl_"

// tokenValueBytes is the random entropy per token (32 bytes → 64 hex).
const tokenValueBytes = 32

// tokenFileMode is the backing file's permissions (the hashes are
// credentials — owner-only).
const tokenFileMode = 0o600

// NewTokenStore loads the token file at path ("" = in-memory). A
// missing file starts an empty store; a corrupt one is a hard error
// (the operator owns the file).
func NewTokenStore(path string, clock func() time.Time) (*TokenStore, error) {
	ts := &TokenStore{path: path, clock: clock, tokens: make(map[string]Token)}
	if path == "" {
		return ts, nil
	}
	//nolint:gosec // G304: the path is operator-configured, not user input.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ts, nil
		}
		return nil, fmt.Errorf("tokens: read %s: %w", path, err)
	}
	var f tokenFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("tokens: parse %s: %w", path, err)
	}
	if f.Version != tokenFileVersion {
		return nil, fmt.Errorf("tokens: %s: unsupported version %d", path, f.Version)
	}
	for i := range f.Tokens {
		tk := f.Tokens[i]
		if tk.ID == "" || tk.Hash == "" || roleLevel[tk.Role] == 0 {
			continue
		}
		ts.tokens[tk.ID] = tk
		if n := tokenSeqOf(tk.ID); n > ts.seq {
			ts.seq = n
		}
	}
	return ts, nil
}

// Create mints one token: the plaintext (fl_ + 32 random bytes hex) is
// returned exactly once; the backing file holds the SHA-256 hash.
func (ts *TokenStore) Create(name string, role Role) (Token, string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if name == "" {
		return Token{}, "", fmt.Errorf("%w: empty name", errTokenName)
	}
	if roleLevel[role] == 0 {
		return Token{}, "", fmt.Errorf("%w: %q", errUnknownRole, role)
	}
	for i := range ts.tokens {
		if tk := ts.tokens[i]; tk.Name == name {
			return Token{}, "", fmt.Errorf("%w: %q already exists", errTokenName, name)
		}
	}
	raw := make([]byte, tokenValueBytes)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, "", fmt.Errorf("tokens: rand: %w", err)
	}
	plaintext := tokenValuePrefix + hex.EncodeToString(raw)
	ts.seq++
	tk := Token{
		ID:      fmt.Sprintf("t%d", ts.seq),
		Name:    name,
		Role:    role,
		Hash:    hashToken(plaintext),
		Created: ts.clock(),
	}
	ts.tokens[tk.ID] = tk
	if err := ts.persistLocked(); err != nil {
		delete(ts.tokens, tk.ID)
		ts.seq--
		return Token{}, "", err
	}
	return tk, plaintext, nil
}

// Lookup authenticates one plaintext token (hash-only — the plaintext
// never touches the store); a valid match updates last_used (persisted
// on the next mutation or FlushUsage, not per request).
func (ts *TokenStore) Lookup(plaintext string) (Token, bool) {
	if plaintext == "" {
		return Token{}, false
	}
	h := hashToken(plaintext)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for id, tk := range ts.tokens {
		if tk.Hash == h {
			now := ts.clock()
			tk.LastUsed = &now
			ts.tokens[id] = tk
			ts.usedDirty = true
			return tk, true
		}
	}
	return Token{}, false
}

// FlushUsage persists last_used when a lookup changed it since the last
// write (audit S11). The host calls it on graceful shutdown — not on a
// timer: a periodic rewrite from memory would drop a token minted with
// the CLI while the service runs.
func (ts *TokenStore) FlushUsage() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !ts.usedDirty {
		return nil
	}
	return ts.persistLocked()
}

// Revoke removes one token (unknown id → errTokenNotFound).
func (ts *TokenStore) Revoke(id string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if _, ok := ts.tokens[id]; !ok {
		return errTokenNotFound
	}
	delete(ts.tokens, id)
	return ts.persistLocked()
}

// HasInstaller reports whether at least one installer-role token exists
// (the run gate: without one, minting further tokens is impossible —
// the service would start locked out).
func (ts *TokenStore) HasInstaller() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, tk := range ts.tokens {
		if tk.Role == RoleInstaller {
			return true
		}
	}
	return false
}

// List returns the tokens (no secrets), oldest first.
func (ts *TokenStore) List() []Token {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := ts.listLocked()
	sort.Slice(out, func(i, j int) bool {
		return tokenSeqOf(out[i].ID) < tokenSeqOf(out[j].ID)
	})
	return out
}

// persistLocked rewrites the file atomically (tmp + rename); the
// in-memory mode is a no-op. The caller holds the store lock.
func (ts *TokenStore) persistLocked() error {
	if ts.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(tokenFile{
		Version: tokenFileVersion,
		Tokens:  ts.listLocked(),
	}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := ts.path + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		return fmt.Errorf("tokens: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, ts.path); err != nil {
		return fmt.Errorf("tokens: rename %s: %w", tmp, err)
	}
	// the rename itself reaches the disk only with the directory synced
	// (a power cut must not leave the file missing or empty — audit R9)
	if d, err := os.Open(filepath.Dir(ts.path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	ts.usedDirty = false
	return nil
}

// writeSynced writes data and syncs it to the disk before returning.
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, tokenFileMode) //nolint:gosec // G304: the operator's token path
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// listLocked copies the tokens (the caller holds the store lock).
func (ts *TokenStore) listLocked() []Token {
	out := make([]Token, 0, len(ts.tokens))
	for i := range ts.tokens {
		out = append(out, ts.tokens[i])
	}
	return out
}

// hashToken is the stored form: "sha256:" + lowercase hex (the
// plaintext is never persisted).
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// tokenSeqOf parses the numeric id suffix ("t3" → 3).
func tokenSeqOf(id string) int {
	n := 0
	for i := len(id) - 1; i >= 0; i-- {
		c := id[i]
		if c < '0' || c > '9' {
			break
		}
		n = int(c-'0') + n*10
	}
	return n
}
