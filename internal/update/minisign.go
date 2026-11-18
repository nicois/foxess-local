package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// ErrBadSignature is a signature that does not verify against the key.
var ErrBadSignature = errors.New("update: signature does not verify")

// minisign layout (https://jedisct1.github.io/minisign/).
const (
	algLen      = 2
	keyIDLen    = 8
	pubRawLen   = algLen + keyIDLen + ed25519.PublicKeySize
	sigRawLen   = algLen + keyIDLen + ed25519.SignatureSize
	algLegacy   = "Ed" // signature over the message
	algHashed   = "ED" // signature over BLAKE2b-512(message): minisign's default
	trustedPfx  = "trusted comment: "
	sigFileRows = 4
)

// PublicKey is a minisign public key.
type PublicKey struct {
	ID  [keyIDLen]byte
	Key ed25519.PublicKey
}

// ParsePublicKey reads a minisign .pub file (or its bare base64 line).
func ParsePublicKey(text string) (PublicKey, error) {
	var pk PublicKey
	var line string
	for _, l := range splitLines(text) {
		if l != "" && !strings.HasPrefix(l, "untrusted comment:") {
			line = l
			break
		}
	}
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil || len(raw) != pubRawLen || string(raw[:algLen]) != algLegacy {
		return pk, errors.New("update: not a minisign public key")
	}
	copy(pk.ID[:], raw[algLen:algLen+keyIDLen])
	pk.Key = ed25519.PublicKey(append([]byte(nil), raw[algLen+keyIDLen:]...))
	return pk, nil
}

// Verify checks a minisign signature file over msg: the key ID, the
// signature (legacy or prehashed), and the global signature binding
// the trusted comment.
func (pk PublicKey) Verify(msg, sigFile []byte) error {
	lines := splitLines(string(sigFile))
	if len(lines) < sigFileRows || !strings.HasPrefix(lines[2], trustedPfx) {
		return fmt.Errorf("%w: malformed signature file", ErrBadSignature)
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(raw) != sigRawLen {
		return fmt.Errorf("%w: malformed signature", ErrBadSignature)
	}
	alg, id, sig := string(raw[:algLen]), raw[algLen:algLen+keyIDLen], raw[algLen+keyIDLen:]
	if !bytes.Equal(id, pk.ID[:]) {
		return fmt.Errorf("%w: signed by another key (id %x)", ErrBadSignature, id)
	}
	data := msg
	switch alg {
	case algHashed:
		h := blake2b.Sum512(msg)
		data = h[:]
	case algLegacy:
	default:
		return fmt.Errorf("%w: unknown algorithm %q", ErrBadSignature, alg)
	}
	if !ed25519.Verify(pk.Key, data, sig) {
		return ErrBadSignature
	}
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(global) != ed25519.SignatureSize {
		return fmt.Errorf("%w: malformed global signature", ErrBadSignature)
	}
	trusted := strings.TrimPrefix(lines[2], trustedPfx)
	if !ed25519.Verify(pk.Key, append(append([]byte(nil), sig...), []byte(trusted)...), global) {
		return fmt.Errorf("%w: trusted comment altered", ErrBadSignature)
	}
	return nil
}

// splitLines splits on newlines, trimming a trailing CR.
func splitLines(s string) []string {
	out := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range out {
		out[i] = strings.TrimRight(out[i], "\r")
	}
	return out
}
