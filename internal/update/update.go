// Package update keeps foxess-local current without root (design
// D-023): it checks the project's GitHub releases, and on an installer's
// request downloads the release binary for this architecture, verifies
// the minisign signature over SHA256SUMS with the public key built into
// this binary and the binary's checksum, self-tests it, and swaps it in
// atomically (the old binary kept as .prev). The caller then exits;
// systemd restarts the service into the new binary, and the root-owned
// upgrade guard restores .prev if the new one keeps failing to start.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"foxesslocal/internal/diskspace"
)

// Refusals.
var (
	ErrNoKey    = errors.New("update: this build carries no release signing key (a source build: upgrade by rebuilding)")
	ErrUpToDate = errors.New("update: already on the latest release")
	ErrNoAsset  = errors.New("update: the release has no binary for this architecture")
	ErrChecksum = errors.New("update: the downloaded binary does not match the signed checksum")
	ErrSelfTest = errors.New("update: the downloaded binary failed its self-test")
	ErrNoRepo   = errors.New("update: no release repository configured")
	ErrNoSpace  = errors.New("update: not enough free disk space for the download")
)

// DefaultAPI is GitHub's REST API.
const DefaultAPI = "https://api.github.com"

// Asset and file names.
const (
	SumsName     = "SHA256SUMS"
	SigName      = "SHA256SUMS.minisig"
	prevSuffix   = ".prev"
	maxDownload  = 200 << 20 // a release binary is ~15 MB
	maxMetadata  = 1 << 20
	selfTestWait = 10 * time.Second
	binMode      = 0o755
)

// AssetName is the release binary for a GOARCH (arm = ARMv7).
func AssetName(arch string) string {
	if arch == "arm" {
		arch = "armv7"
	}
	return "foxess-local_linux_" + arch
}

// Updater checks and applies releases.
type Updater struct {
	Repo     string // "owner/name"
	API      string // DefaultAPI unless testing
	Client   *http.Client
	Key      *PublicKey // nil: this build cannot verify releases
	Current  string     // this binary's version ("v0.1.0", or a git hash for source builds)
	Arch     string     // runtime.GOARCH
	Exe      string     // the installed binary's path
	SelfTest func(ctx context.Context, path string) (string, error)
	// FreeSpace reports the free and total bytes of the filesystem
	// holding a directory (nil, or an error from it: not checked).
	FreeSpace func(dir string) (free, total uint64, err error)
	// MinFree is the critical free-space level kept after the download
	// ([retention] min_free_mb; diskspace.Threshold caps it).
	MinFree uint64
	// Prerelease reports whether prereleases may be offered (the
	// installer's opt-in, read at every check; nil: never).
	Prerelease func() bool
}

// allowPre is the prerelease opt-in now.
func (u *Updater) allowPre() bool { return u.Prerelease != nil && u.Prerelease() }

// eligible: a release this installation may be offered (not a draft; a
// prerelease only when opted in; a version tag newer than this one).
func (u *Updater) eligible(r release) bool {
	return !r.Draft && (!r.Prerelease || u.allowPre()) && Newer(r.Tag, u.Current)
}

// Status is the result of a check.
type Status struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	URL       string `json:"url"`
	Available bool   `json:"available"`
	// Prerelease: Latest is a prerelease (offered because of the opt-in).
	Prerelease bool      `json:"prerelease"`
	CheckedAt  time.Time `json:"checked_at"`
}

type release struct {
	Tag        string `json:"tag_name"`
	URL        string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size uint64 `json:"size"`
	} `json:"assets"`
}

func (r release) asset(name string) (string, bool) {
	url, _, ok := r.assetSized(name)
	return url, ok
}

// assetSized is an asset's URL and size (0 when the API omits it).
func (r release) assetSized(name string) (string, uint64, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, a.Size, true
		}
	}
	return "", 0, false
}

// semver is a release tag: vMAJOR.MINOR.PATCH with an optional
// prerelease (-rc.1, -beta, …; dot-separated alphanumeric identifiers).
var semver = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// IsPrerelease reports whether a release tag is a prerelease.
func IsPrerelease(tag string) bool {
	m := semver.FindStringSubmatch(tag)
	return m != nil && m[4] != ""
}

// Newer reports whether latest is a newer release than current, in
// semver order (a prerelease comes before its release). A current
// version that is not a release tag (a source build) is never offered an
// update. Whether a prerelease may be offered at all is the caller's.
func Newer(latest, current string) bool {
	l, c := semver.FindStringSubmatch(latest), semver.FindStringSubmatch(current)
	if l == nil || c == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		a, _ := strconv.Atoi(l[i])
		b, _ := strconv.Atoi(c[i])
		if a != b {
			return a > b
		}
	}
	return comparePre(l[4], c[4]) > 0
}

// comparePre orders two prerelease strings ("" = a release, which comes
// after any prerelease of the same version): identifier by identifier,
// numbers numerically and before words, a longer list after its prefix.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(as), len(bs)) {
		if c := compareIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return len(as) - len(bs)
}

func compareIdent(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		return na - nb
	case ea == nil:
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func (u *Updater) api() string {
	if u.API != "" {
		return u.API
	}
	return DefaultAPI
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// no identifying data: the version only
	req.Header.Set("User-Agent", "foxess-local/"+u.Current)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("update: %s exceeds %d bytes", url, limit)
	}
	return b, nil
}

func (u *Updater) latest(ctx context.Context) (release, error) {
	var r release
	if u.Repo == "" {
		return r, ErrNoRepo
	}
	if u.allowPre() {
		return u.newestListed(ctx)
	}
	b, err := u.get(ctx, u.api()+"/repos/"+u.Repo+"/releases/latest", maxMetadata)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("update: release metadata: %w", err)
	}
	return r, nil
}

// listedReleases is how many recent releases the prerelease channel looks
// at (GitHub lists newest first).
const listedReleases = 30

// newestListed is the newest release by version among the recent ones
// (GitHub's "latest" never names a prerelease): drafts and malformed tags
// skipped. An empty listing is the zero release (nothing to offer).
func (u *Updater) newestListed(ctx context.Context) (release, error) {
	b, err := u.get(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=%d", u.api(), u.Repo, listedReleases), maxMetadata)
	if err != nil {
		return release{}, err
	}
	var list []release
	if err := json.Unmarshal(b, &list); err != nil {
		return release{}, fmt.Errorf("update: release list: %w", err)
	}
	var best release
	for _, r := range list {
		if r.Draft || !semver.MatchString(r.Tag) {
			continue
		}
		if best.Tag == "" || Newer(r.Tag, best.Tag) {
			best = r
		}
	}
	return best, nil
}

// Check asks for the latest release.
func (u *Updater) Check(ctx context.Context) (Status, error) {
	st := Status{Current: u.Current, CheckedAt: time.Now()}
	r, err := u.latest(ctx)
	if err != nil {
		return st, err
	}
	st.Latest, st.URL, st.Prerelease = r.Tag, r.URL, r.Prerelease
	st.Available = u.eligible(r)
	return st, nil
}

// Apply installs the latest release (see the package comment). On any
// failure the installed binary is untouched and nothing is left behind.
func (u *Updater) Apply(ctx context.Context) error {
	if u.Key == nil {
		return ErrNoKey
	}
	r, err := u.latest(ctx)
	if err != nil {
		return err
	}
	if !u.eligible(r) {
		return ErrUpToDate
	}
	want, err := u.signedHash(ctx, r)
	if err != nil {
		return err
	}
	binURL, size, ok := r.assetSized(AssetName(u.Arch))
	if !ok {
		return ErrNoAsset
	}
	if err := u.checkSpace(size); err != nil {
		return err
	}
	bin, err := u.get(ctx, binURL, maxDownload)
	if err != nil {
		return err
	}
	if h := sha256.Sum256(bin); hex.EncodeToString(h[:]) != want {
		return ErrChecksum
	}
	return u.install(ctx, bin, r.Tag)
}

// checkSpace refuses a download that would leave the binary's filesystem
// below the critical level (an unknown size counts as the download cap).
func (u *Updater) checkSpace(size uint64) error {
	if u.FreeSpace == nil {
		return nil
	}
	free, total, err := u.FreeSpace(filepath.Dir(u.Exe))
	if err != nil {
		return nil //nolint:nilerr // unsupported platform: not checked
	}
	if size == 0 {
		size = maxDownload
	}
	if need := size + diskspace.Threshold(u.MinFree, total); free < need {
		return fmt.Errorf("%w: %d MB free, %d MB needed", ErrNoSpace, free/diskspace.MiB, need/diskspace.MiB)
	}
	return nil
}

// signedHash verifies SHA256SUMS against the built-in key and returns
// this architecture's line.
func (u *Updater) signedHash(ctx context.Context, r release) (string, error) {
	sumsURL, ok1 := r.asset(SumsName)
	sigURL, ok2 := r.asset(SigName)
	if !ok1 || !ok2 {
		return "", fmt.Errorf("%w (no %s / %s)", ErrNoAsset, SumsName, SigName)
	}
	sums, err := u.get(ctx, sumsURL, maxMetadata)
	if err != nil {
		return "", err
	}
	sig, err := u.get(ctx, sigURL, maxMetadata)
	if err != nil {
		return "", err
	}
	if err := u.Key.Verify(sums, sig); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == AssetName(u.Arch) {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", ErrNoAsset
}

// install writes the verified binary next to the installed one,
// self-tests it, and swaps it in: the installed binary is hard-linked
// to .prev, then the new one is renamed over it (never a moment without
// a runnable binary in place).
func (u *Updater) install(ctx context.Context, bin []byte, tag string) error {
	dir := filepath.Dir(u.Exe)
	tmp, err := os.CreateTemp(dir, ".foxess-local.new.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(e error) error { _ = os.Remove(tmpName); return e }
	if _, err := tmp.Write(bin); err != nil {
		_ = tmp.Close()
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := os.Chmod(tmpName, binMode); err != nil {
		return cleanup(err)
	}
	test := u.SelfTest
	if test == nil {
		test = execSelfTest
	}
	if v, err := test(ctx, tmpName); err != nil || v != tag {
		return cleanup(fmt.Errorf("%w (reported %q, want %s: %w)", ErrSelfTest, v, tag, err))
	}
	prev := u.Exe + prevSuffix
	_ = os.Remove(prev)
	if err := os.Link(u.Exe, prev); err != nil {
		return cleanup(fmt.Errorf("update: keep the previous binary: %w", err))
	}
	if err := os.Rename(tmpName, u.Exe); err != nil {
		_ = os.Remove(prev)
		return cleanup(err)
	}
	return WriteMarker(dir, Marker{From: u.Current, To: tag})
}

// execSelfTest runs `<binary> version`.
func execSelfTest(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, selfTestWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	return strings.TrimSpace(string(out)), err
}
