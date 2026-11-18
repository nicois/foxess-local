package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// testKey makes a minisign key pair; pubText is the .pub file.
func testKey(t *testing.T) (ed25519.PrivateKey, [8]byte, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	raw := append(append([]byte("Ed"), id[:]...), pub...)
	return priv, id, "untrusted comment: minisign public key\n" + base64.StdEncoding.EncodeToString(raw) + "\n"
}

// sign makes a minisign signature file over msg (alg "Ed" legacy or
// "ED" prehashed).
func sign(priv ed25519.PrivateKey, id [8]byte, alg string, msg []byte, trusted string) string {
	data := msg
	if alg == "ED" {
		h := blake2b.Sum512(msg)
		data = h[:]
	}
	sig := ed25519.Sign(priv, data)
	line := append(append([]byte(alg), id[:]...), sig...)
	global := ed25519.Sign(priv, append(append([]byte(nil), sig...), []byte(trusted)...))
	return fmt.Sprintf("untrusted comment: signature\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(line), trusted, base64.StdEncoding.EncodeToString(global))
}

// TestMinisignVerify: both signature kinds verify; a changed message, a
// changed trusted comment, or another key's signature do not.
func TestMinisignVerify(t *testing.T) {
	priv, id, pubText := testKey(t)
	pk, err := ParsePublicKey(pubText)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("SHA256SUMS contents\n")
	for _, alg := range []string{"Ed", "ED"} {
		s := sign(priv, id, alg, msg, "timestamp:1 file:SHA256SUMS")
		if err := pk.Verify(msg, []byte(s)); err != nil {
			t.Errorf("%s: %v", alg, err)
		}
		if err := pk.Verify([]byte("tampered\n"), []byte(s)); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s tampered message: %v", alg, err)
		}
	}
	good := sign(priv, id, "ED", msg, "timestamp:1")
	forged := []byte(good[:len(good)-1]) // keep structure, then swap the trusted comment
	forged = []byte(replaceLine(string(forged), 2, "trusted comment: timestamp:2"))
	if err := pk.Verify(msg, forged); !errors.Is(err, ErrBadSignature) {
		t.Errorf("changed trusted comment: %v", err)
	}
	other, oid, _ := testKey(t)
	if err := pk.Verify(msg, []byte(sign(other, oid, "ED", msg, "x"))); !errors.Is(err, ErrBadSignature) {
		t.Errorf("another key: %v", err)
	}
	if _, err := ParsePublicKey("untrusted comment: x\nnot-base64!!\n"); err == nil {
		t.Error("garbage public key parsed")
	}
}

func replaceLine(s string, n int, with string) string {
	lines := splitLines(s)
	lines[n] = with
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
