// Package deploy holds the gateway deploy artifacts (gate scripts,
// systemd units, the installer generator) and embeds the one artifact
// the binary itself needs: the FoxESS cloud's vendor self-signed root.
package deploy

import _ "embed" // go:embed

// CloudRootPEM is the vendor self-signed certificate the real cloud
// presents on the upstream leg (pinned by TestCloudRootPEM). The app
// trusts it by default for telemetry_only/full; forward.upstream_cas
// overrides it.
//
//go:embed cloud-root.pem
var CloudRootPEM []byte

// ReleaseKey is the minisign public key release signatures are checked
// against by the self-upgrade (internal/update). Empty in a source build:
// self-upgrade is then refused (rebuild to upgrade). The release owner
// generates the key pair (minisign -G) and commits only the .pub here.
//
//go:embed release-key.pub
var ReleaseKey []byte
