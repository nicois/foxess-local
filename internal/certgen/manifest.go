package certgen

import (
	"encoding/json"
	"os"
	"time"
)

// manifestMode is the permission for the manifest file: it records which
// anchor the broker trusts, so it stays operator-local.
const manifestMode = 0o600

// Manifest records which trust anchor actually produced the live broker cert.
// The broker refuses to start without one (spec §5: anchor switch is
// certgen + restart, no code change).
type Manifest struct {
	AnchorKind    AnchorKind `json:"anchor_kind"`
	IssuerSubject string     `json:"issuer_subject,omitempty"`
	SmallKey      int        `json:"small_key,omitempty"`
	SANHosts      []string   `json:"san_hosts"`
	SANIPs        []string   `json:"san_ips"`
	NotBefore     time.Time  `json:"not_before"`
	NotAfter      time.Time  `json:"not_after"`
	CreatedAt     time.Time  `json:"created_at"`
	CertlabOK     bool       `json:"certlab_ok"`
}

// WriteManifest writes m as indented JSON at path with 0600 permissions.
func WriteManifest(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, manifestMode)
}

// LoadManifest reads and parses the manifest JSON at path.
func LoadManifest(path string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(path) //nolint:gosec // G304: the manifest path is operator-configured at startup
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}
