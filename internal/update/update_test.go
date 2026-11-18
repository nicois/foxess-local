package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Test versions and repo.
const (
	vOld     = "v0.1.0"
	vNew     = "v0.2.0"
	testRepo = "o/r"
	dlKey    = "browser_download_url"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{vNew, "v0.1.9", true},
		{"v0.10.0", "v0.9.3", true},
		{"v1.0.0", "v1.0.0", false},
		{vOld, vNew, false},
		{vNew, "92ca87b", false},     // a source build: no comparison
		{vNew + "-rc1", vOld, false}, // pre-releases are not offered
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.latest, c.current, got)
		}
	}
}

// oldBin is the fake installed binary's content.
const oldBin = "OLD"

// fakeGitHub serves a latest release with the three assets.
type fakeGitHub struct {
	tag       string
	bin, sums []byte
	sig       string
}

func (f *fakeGitHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/foxess-local/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": f.tag, "html_url": "https://example.invalid/release", "draft": false, "prerelease": false,
				"assets": []map[string]string{
					asset(AssetName("arm64"), srv.URL+"/dl/bin"),
					asset(SumsName, srv.URL+"/dl/sums"),
					asset(SigName, srv.URL+"/dl/sig"),
				},
			})
		case "/dl/bin":
			_, _ = w.Write(f.bin)
		case "/dl/sums":
			_, _ = w.Write(f.sums)
		case "/dl/sig":
			_, _ = w.Write([]byte(f.sig))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// install makes a fake installed binary in a temp bin dir.
func install(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "foxess-local")
	if err := os.WriteFile(exe, []byte(oldBin), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	return exe
}

func setup(t *testing.T, tamper func(f *fakeGitHub)) (*Updater, string) {
	t.Helper()
	priv, id, pubText := testKey(t)
	pk, _ := ParsePublicKey(pubText)
	bin := []byte("NEW BINARY " + vNew)
	h := sha256.Sum256(bin)
	sums := []byte(fmt.Sprintf("%s  %s\n%s  installer.sh\n", hex.EncodeToString(h[:]), AssetName("arm64"), hex.EncodeToString(make([]byte, 32))))
	f := &fakeGitHub{tag: vNew, bin: bin, sums: sums, sig: sign(priv, id, "ED", sums, "file:SHA256SUMS")}
	if tamper != nil {
		tamper(f)
	}
	srv := f.server(t)
	exe := install(t)
	u := &Updater{
		Repo: "owner/foxess-local", API: srv.URL, Client: srv.Client(), Key: &pk,
		Current: vOld, Arch: "arm64", Exe: exe,
		SelfTest: func(_ context.Context, path string) (string, error) {
			b, err := os.ReadFile(path) //nolint:gosec // test temp file
			if err != nil {
				return "", err
			}
			if string(b) != "NEW BINARY "+vNew {
				return "", errors.New("not the new binary")
			}
			return vNew, nil
		},
	}
	return u, exe
}

// TestCheckAndApply: the check reports the newer release; apply
// verifies the signed checksums, self-tests the new binary, swaps it in
// atomically keeping the old one as .prev, and leaves the pending marker
// for the restart guard.
func TestCheckAndApply(t *testing.T) {
	u, exe := setup(t, nil)
	st, err := u.Check(context.Background())
	if err != nil || !st.Available || st.Latest != vNew {
		t.Fatalf("check: %+v %v", st, err)
	}
	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b := readFile(exe); string(b) != "NEW BINARY "+vNew {
		t.Fatalf("binary not swapped: %q", b)
	}
	if b := readFile(exe + ".prev"); string(b) != oldBin {
		t.Fatalf(".prev: %q", b)
	}
	m, ok, err := ReadMarker(filepath.Dir(exe))
	if err != nil || !ok || m.From != vOld || m.To != vNew {
		t.Fatalf("marker: %+v %v %v", m, ok, err)
	}
	if err := ConfirmHealthy(filepath.Dir(exe)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ReadMarker(filepath.Dir(exe)); ok {
		t.Fatal("marker survives confirmation")
	}
}

// TestApplyRefusals: nothing on disk changes when the signature, the
// checksum or the self-test fails, or the build carries no release key.
func TestApplyRefusals(t *testing.T) {
	cases := map[string]struct {
		tamper func(f *fakeGitHub)
		mutate func(u *Updater)
		want   error
	}{
		"bad signature": {tamper: func(f *fakeGitHub) { f.sums = append(f.sums, '\n') }, want: ErrBadSignature},
		"hash mismatch": {tamper: func(f *fakeGitHub) { f.bin = []byte("NEW BINARY " + vNew + " + backdoor") }, want: ErrChecksum},
		"self-test":     {mutate: func(u *Updater) { u.SelfTest = func(context.Context, string) (string, error) { return "v0.1.5", nil } }, want: ErrSelfTest},
		"no key":        {mutate: func(u *Updater) { u.Key = nil }, want: ErrNoKey},
		"not newer":     {mutate: func(u *Updater) { u.Current = vNew }, want: ErrUpToDate},
	}
	for name, c := range cases {
		u, exe := setup(t, c.tamper)
		if c.mutate != nil {
			c.mutate(u)
		}
		if err := u.Apply(context.Background()); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if b := readFile(exe); string(b) != oldBin {
			t.Errorf("%s: installed binary changed", name)
		}
		entries, _ := os.ReadDir(filepath.Dir(exe))
		if len(entries) != 1 {
			t.Errorf("%s: leftovers in the bin dir: %v", name, entries)
		}
	}
}

// readFile reads a test temp file ("" when absent).
func readFile(path string) []byte {
	b, _ := os.ReadFile(path) //nolint:gosec // test temp dir
	return b
}

// asset is one release asset in GitHub's JSON shape.
func asset(name, url string) map[string]string {
	return map[string]string{"name": name, dlKey: url}
}

// TestApplyNeedsFreeSpace: an upgrade checks the binary's filesystem
// before downloading — the asset plus the critical level must fit — and
// refuses without fetching the binary when they do not.
func TestApplyNeedsFreeSpace(t *testing.T) {
	u, exe := setup(t, nil)
	var fetched bool
	inner := u.Client.Transport
	u.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/dl/bin" {
			fetched = true
		}
		return inner.RoundTrip(r)
	})
	u.MinFree = 200 << 20
	u.FreeSpace = func(dir string) (uint64, uint64, error) {
		if dir != filepath.Dir(exe) {
			t.Errorf("checked %s, want the binary's directory", dir)
		}
		return 100 << 20, 32 << 30, nil
	}
	if err := u.Apply(context.Background()); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("apply with 100 MB free: %v, want ErrNoSpace", err)
	}
	if fetched {
		t.Fatal("the binary was downloaded despite the space check")
	}
	if b, _ := os.ReadFile(exe); string(b) != oldBin { //nolint:gosec // test temp file
		t.Fatal("the installed binary changed")
	}

	u.FreeSpace = func(string) (uint64, uint64, error) { return 1 << 30, 32 << 30, nil }
	if err := u.Apply(context.Background()); err != nil {
		t.Fatalf("apply with 1 GB free: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
