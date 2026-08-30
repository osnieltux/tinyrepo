package tinyrepo

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// indexFixture is a mirror that serves whatever body is registered for each
// compression variant, so a test can make one of them broken and leave the rest
// intact.
type indexFixture struct {
	bodies  map[string][]byte // "Packages.xz" -> its bytes
	release *releaseIndex
	target  indexTarget
	root    string
}

func newIndexFixture(t *testing.T, bodies map[string][]byte) *indexFixture {
	t.Helper()

	fixture := &indexFixture{
		bodies:  bodies,
		release: &releaseIndex{Files: map[string]fileCheck{}},
		root:    t.TempDir(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		body, ok := fixture.bodies[name]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(server.Close)

	fixture.target = indexTarget{BaseURL: server.URL, Dist: "bookworm", Component: "main", Arch: "amd64"}
	return fixture
}

// publish records in Release what the mirror is expected to serve under name.
// Passing different bytes from the ones the mirror serves is how a tampered or
// corrupt variant is expressed.
func (f *indexFixture) publish(name string, body []byte) {
	sum := sha256.Sum256(body)
	f.release.Files["main/binary-amd64/"+name] = fileCheck{
		SHA256: hex.EncodeToString(sum[:]),
		Size:   int64(len(body)),
	}
}

func (f *indexFixture) fetch() error {
	return fetchTargetIndex(f.root, f.target, f.release)
}

// packages returns the expanded index left in the cache.
func (f *indexFixture) packages(t *testing.T) (string, bool) {
	t.Helper()

	path := filepath.Join(f.target.cacheDir(f.root), "Packages")
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(body), true
}

func gzipBytes(t *testing.T, body []byte) []byte {
	t.Helper()

	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func xzBytes(t *testing.T, body []byte) []byte {
	t.Helper()

	var out bytes.Buffer
	writer, err := xz.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

const indexBody = "Package: nano\nVersion: 7.2-1\nFilename: pool/main/n/nano/nano_7.2-1_amd64.deb\n\n"

// A variant that downloads cleanly and then does not expand is as useless as one
// that 404s, and the mirror usually publishes a perfectly good .gz beside it.
func TestFetchTargetIndexFallsBackWhenTheXZIsCorrupt(t *testing.T) {
	good := []byte(indexBody)
	// Announced as .xz, and it is not: a truncated or mislabelled file.
	broken := []byte("this is not xz at all")

	fixture := newIndexFixture(t, map[string][]byte{
		"Packages.xz": broken,
		"Packages.gz": gzipBytes(t, good),
	})
	// Release describes what the mirror actually serves, so the download itself
	// succeeds and only the expansion fails.
	fixture.publish("Packages", good)
	fixture.publish("Packages.xz", broken)
	fixture.publish("Packages.gz", fixture.bodies["Packages.gz"])

	if err := fixture.fetch(); err != nil {
		t.Fatalf("a broken .xz was not stepped over: %v", err)
	}

	body, ok := fixture.packages(t)
	if !ok {
		t.Fatal("no Packages was written")
	}
	if body != indexBody {
		t.Errorf("Packages = %q, want the .gz contents %q", body, indexBody)
	}
}

// The download matching Release proves the transfer; the expansion is what
// everything downstream reads, so a mismatch there has to fall back too.
func TestFetchTargetIndexFallsBackWhenTheExpansionDoesNotMatch(t *testing.T) {
	good := []byte(indexBody)
	// Valid xz, so it decompresses; the wrong contents, so it fails the check.
	wrong := xzBytes(t, []byte("Package: trojan\nVersion: 1\n\n"))

	fixture := newIndexFixture(t, map[string][]byte{
		"Packages.xz": wrong,
		"Packages.gz": gzipBytes(t, good),
	})
	fixture.publish("Packages", good)
	fixture.publish("Packages.xz", wrong)
	fixture.publish("Packages.gz", fixture.bodies["Packages.gz"])

	if err := fixture.fetch(); err != nil {
		t.Fatalf("an .xz expanding to the wrong index was not stepped over: %v", err)
	}

	body, ok := fixture.packages(t)
	if !ok {
		t.Fatal("no Packages was written")
	}
	if body != indexBody {
		t.Errorf("Packages = %q, want the .gz contents %q", body, indexBody)
	}
}

// Falling back must not leave the rejected expansion behind: -ci reads that file
// without rechecking it, so a wrong one that survives is worse than none.
func TestFetchTargetIndexRemovesARejectedExpansion(t *testing.T) {
	good := []byte(indexBody)
	wrong := xzBytes(t, []byte("Package: trojan\nVersion: 1\n\n"))

	// Only the .xz is served, so there is nothing to fall back to.
	fixture := newIndexFixture(t, map[string][]byte{"Packages.xz": wrong})
	fixture.publish("Packages", good)
	fixture.publish("Packages.xz", wrong)

	if err := fixture.fetch(); err == nil {
		t.Fatal("every variant failed and fetchTargetIndex reported success")
	}

	if body, ok := fixture.packages(t); ok {
		t.Errorf("the rejected expansion was left in the cache: %q", body)
	}
}

// The fallback must not turn a mirror that serves nothing usable into a silent
// success.
func TestFetchTargetIndexStillFailsWhenEveryVariantIsBroken(t *testing.T) {
	fixture := newIndexFixture(t, map[string][]byte{
		"Packages.xz": []byte("not xz"),
		"Packages.gz": []byte("not gzip"),
	})
	fixture.publish("Packages.xz", fixture.bodies["Packages.xz"])
	fixture.publish("Packages.gz", fixture.bodies["Packages.gz"])

	if err := fixture.fetch(); err == nil {
		t.Error("a mirror serving nothing usable was accepted")
	}
}

// The common path has to stay exactly as it was: a good .xz is used, and the
// .gz is never requested.
func TestFetchTargetIndexStopsAtTheFirstUsableVariant(t *testing.T) {
	good := []byte(indexBody)

	fixture := newIndexFixture(t, map[string][]byte{
		"Packages.xz": xzBytes(t, good),
		"Packages.gz": gzipBytes(t, []byte("Package: never-read\n\n")),
	})
	fixture.publish("Packages", good)
	fixture.publish("Packages.xz", fixture.bodies["Packages.xz"])

	if err := fixture.fetch(); err != nil {
		t.Fatal(err)
	}

	body, ok := fixture.packages(t)
	if !ok {
		t.Fatal("no Packages was written")
	}
	if body != indexBody {
		t.Errorf("Packages = %q, want the .xz contents %q", body, indexBody)
	}
	if _, err := os.Stat(filepath.Join(fixture.target.cacheDir(fixture.root), "Packages.gz")); err == nil {
		t.Error("the .gz was fetched even though the .xz was good")
	}
}
