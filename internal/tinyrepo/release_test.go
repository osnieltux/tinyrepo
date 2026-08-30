package tinyrepo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A trimmed Release with the shape the real ones have: several checksum blocks,
// only one of which is the SHA256 we want, and ordinary fields around them.
const sampleRelease = `Origin: Debian
Label: Debian
Suite: stable
Codename: bookworm
Architectures: amd64 i386
Components: main contrib
Acquire-By-Hash: yes
MD5Sum:
 d41d8cd98f00b204e9800998ecf8427e 50060337 main/binary-amd64/Packages
 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 12084798 main/binary-amd64/Packages.gz
SHA256:
 515e692f2c4121c6fcec444ef100cc18f79a991910615f3a88c8b7becfc94d2f 50060337 main/binary-amd64/Packages
 6777ea16514725f9427f48c240d92e0a6b8496138e37a1491d8597e66774e309 12084798 main/binary-amd64/Packages.gz
 9e0b5aabb2465b3d2e7a7fe27f9913846277833f7a2826e7767acccff5b588c5  8790396 main/binary-amd64/Packages.xz
 1111111111111111111111111111111111111111111111111111111111111111   123456 contrib/binary-amd64/Packages.xz
Description: Debian 12
`

func TestParseReleaseIndex(t *testing.T) {
	index, err := parseReleaseIndex(strings.NewReader(sampleRelease))
	if err != nil {
		t.Fatalf("parseReleaseIndex: %v", err)
	}

	tests := []struct {
		component string
		arch      string
		name      string
		wantFound bool
		wantSize  int64
		wantSHA   string
	}{
		{"main", "amd64", "Packages", true, 50060337,
			"515e692f2c4121c6fcec444ef100cc18f79a991910615f3a88c8b7becfc94d2f"},
		{"main", "amd64", "Packages.xz", true, 8790396,
			"9e0b5aabb2465b3d2e7a7fe27f9913846277833f7a2826e7767acccff5b588c5"},
		{"contrib", "amd64", "Packages.xz", true, 123456,
			"1111111111111111111111111111111111111111111111111111111111111111"},
		{"main", "i386", "Packages", false, 0, ""},
		{"nonexistent", "amd64", "Packages", false, 0, ""},
	}

	for _, tc := range tests {
		t.Run(tc.component+"/"+tc.arch+"/"+tc.name, func(t *testing.T) {
			got, ok := index.check(tc.component, tc.arch, tc.name)
			if ok != tc.wantFound {
				t.Fatalf("found = %v, want %v", ok, tc.wantFound)
			}
			if !ok {
				return
			}
			if got.Size != tc.wantSize {
				t.Errorf("size = %d, want %d", got.Size, tc.wantSize)
			}
			if got.SHA256 != tc.wantSHA {
				t.Errorf("sha256 = %s, want %s", got.SHA256, tc.wantSHA)
			}
		})
	}
}

// The MD5Sum block has the same shape as the SHA256 one. Reading the wrong
// block would hand a 32-hex-digit MD5 to a SHA256 comparison, which would then
// reject every index the mirror serves.
func TestParseReleaseIndexIgnoresWeakerDigests(t *testing.T) {
	index, err := parseReleaseIndex(strings.NewReader(sampleRelease))
	if err != nil {
		t.Fatalf("parseReleaseIndex: %v", err)
	}

	want, ok := index.check("main", "amd64", "Packages")
	if !ok {
		t.Fatal("main/binary-amd64/Packages is missing")
	}
	if len(want.SHA256) != 64 {
		t.Errorf("checksum %q is %d characters, so it did not come from the SHA256 block",
			want.SHA256, len(want.SHA256))
	}
}

func TestParseReleaseIndexRejectsUseless(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{"empty", ""},
		{"no checksum block", "Origin: Debian\nSuite: stable\n"},
		{"only md5", "MD5Sum:\n d41d8cd98f00b204e9800998ecf8427e 10 main/binary-amd64/Packages\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseReleaseIndex(strings.NewReader(tc.contents)); err == nil {
				t.Error("a Release with no usable SHA256 block was accepted")
			}
		})
	}
}

// A nil index is the "mirror published nothing" case, and must not panic.
func TestReleaseIndexCheckOnNil(t *testing.T) {
	var index *releaseIndex

	if _, ok := index.check("main", "amd64", "Packages"); ok {
		t.Error("a missing Release reported a checksum")
	}
}

func TestReleaseURL(t *testing.T) {
	tests := []struct {
		base string
		dist string
		want string
	}{
		{"https://deb.debian.org/debian", "bookworm", "https://deb.debian.org/debian/dists/bookworm/Release"},
		{"https://deb.debian.org/debian/", "bookworm", "https://deb.debian.org/debian/dists/bookworm/Release"},
	}

	for _, tc := range tests {
		t.Run(tc.base, func(t *testing.T) {
			if got := releaseURL(tc.base, tc.dist); got != tc.want {
				t.Errorf("releaseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// A mirror that does not serve Release must produce an error the caller can
// turn into a warning, not a panic and not a silent success.
func TestFetchReleaseIndexReportsAMissingRelease(t *testing.T) {
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mirror.Close()

	if _, err := fetchReleaseIndex(mirror.URL, "bookworm"); err == nil {
		t.Error("a 404 Release was accepted")
	}
}

// An index whose checksum does not match what Release publishes has to be
// rejected: everything built afterwards reads that file.
func TestFetchTargetIndexRejectsATamperedIndex(t *testing.T) {
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Packages") {
			// Not what the Release below promises.
			w.Write([]byte("Package: trojan\nVersion: 1\n\n"))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mirror.Close()

	release := &releaseIndex{Files: map[string]fileCheck{
		"main/binary-amd64/Packages": {
			SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
			Size:   28,
		},
	}}

	target := indexTarget{BaseURL: mirror.URL, Dist: "bookworm", Component: "main", Arch: "amd64"}

	if err := fetchTargetIndex(t.TempDir(), target, release); err == nil {
		t.Error("an index that does not match Release was accepted")
	}
}
