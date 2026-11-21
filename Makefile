GIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: all foxess-local installer fmdata tag release sign verify-release image synthdb pgo
all: foxess-local installer

# The app binary, stamped with the git short hash. The stamp is the
# version reported by the store's connections rows, the API info, and
# the startup log (D-018); an unstamped build reports "dev".
foxess-local:
	go build -ldflags "-X main.version=$(GIT)" -o foxess-local ./cmd/foxess-local

# The standalone installer (installer.sh): embeds the deploy/ scripts,
# gate.nft, the systemd units, and config.example.toml. deploy/ is the
# source of truth — edit there, regenerate here.
installer:
	bash deploy/gen-installer.sh > installer.sh
	chmod +x installer.sh

# Refresh the foxess_modbus register data (design D-021). Pin a reviewed
# upstream commit; the diff of internal/fmdata/foxess_modbus.json is the
# review surface, and TestKnownDivergences / TestWrongMapsRejected must
# still pass. Not part of `all` (needs network).
FM_COMMIT ?= 94252f0610028f323cd86c1c6051b6aeaf8e866d
FM_SRC ?= .cache/foxess_modbus
fmdata:
	test -d $(FM_SRC)/.git || git clone -q https://github.com/nathanmarlor/foxess_modbus $(FM_SRC)
	git -C $(FM_SRC) fetch -q origin
	git -C $(FM_SRC) checkout -q $(FM_COMMIT)
	python3 -I tools/fmimport/fmimport.py --src $(FM_SRC) --out internal/fmdata/foxess_modbus.json

# ---- releases (design D-023; RELEASING.md) ----
#   make tag             # next version from CHANGELOG.md's Unreleased entries;
#                        # commits and tags locally (BUMP=minor|major raises it)
#   make tag PRE=1       # a prerelease of it (vX.Y.Z-rc.N), changelog untouched
#   make release VERSION=v0.2.0 REPO=owner/foxess-local
#   make sign VERSION=v0.2.0  # as the key's account: minisign with SIGN_KEY
#   make verify-release  # the new binary verifies its own release
VERSION ?=
REPO ?=
DIST ?= dist
# The secret key lives in another account; that account runs `make sign`
# (e.g. make -C <this checkout> sign VERSION=…). Override per run.
SIGN_KEY ?= /home/nfarrell/foxess-key/foxess-local.key
BUMP ?=
PRE ?=
tag:
	BUMP="$(BUMP)" PRE="$(PRE)" deploy/tag.sh

release:
	deploy/release.sh "$(VERSION)" "$(REPO)" "$(DIST)"
	# the signer is another account: an existing file it may write is all
	# minisign needs (it truncates in place); a tampered signature fails
	# verify-release, it cannot forge one
	install -m 0666 /dev/null $(DIST)/SHA256SUMS.minisig

sign:
	@test -n "$(VERSION)" || { echo "make sign VERSION=vX.Y.Z" >&2; exit 2; }
	@test -w $(DIST)/SHA256SUMS.minisig || { echo "$(DIST)/SHA256SUMS.minisig is not writable by $$(id -un): run make release first" >&2; exit 2; }
	@bash deploy/sign-check.sh "$(VERSION)" "$(DIST)"
	minisign -S -s $(SIGN_KEY) -m $(DIST)/SHA256SUMS -x $(DIST)/SHA256SUMS.minisig -t "foxess-local $(VERSION)"

verify-release:
	deploy/verify-release.sh "$(DIST)"

# Container image for this machine's architecture from a release build in
# $(DIST) (podman or docker); CI builds the multi-arch image (image.yml).
CONTAINER ?= podman
image:
	deploy/container/prepare.sh $(DIST) $(DIST)/image-ctx
	$(CONTAINER) build -f $(DIST)/image-ctx/Containerfile -t foxess-local:$(VERSION) $(DIST)/image-ctx

# ---- performance (synthetic data, profiles, PGO) ----
#   make synthdb DAYS=30            # build $(SYNTH) at the live rates
#   go run ./tools/devui -db $(SYNTH) -pprof 127.0.0.1:6060
#   go run ./tools/loadgen -token fl_… -dur 60s
# PGO: put CPU profiles from real devices ([debug] pprof_listen,
# /debug/pprof/profile?seconds=600) in pgo/*.pprof, then `make pgo`;
# go build uses cmd/foxess-local/default.pgo automatically.
DAYS ?= 30
SYNTH ?= .cache/synth.db
synthdb:
	mkdir -p $(dir $(SYNTH))
	test ! -e $(SYNTH) || { echo "$(SYNTH) exists"; exit 1; }
	go run ./tools/synthdb -out $(SYNTH) -days $(DAYS)

pgo:
	go tool pprof -proto pgo/*.pprof > cmd/foxess-local/default.pgo
