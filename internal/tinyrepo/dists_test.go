package tinyrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T, architectures ...string) *Config {
	t.Helper()

	config := defaultConfig()
	config.Destination.Path = t.TempDir()
	config.Destination.Arch = architectures
	config.Server.Source = []string{"http://example.org/debian bookworm main"}
	return &config
}

func pkg(name, arch, indexArch string) *PackageDeb {
	return &PackageDeb{
		Package:     name,
		Version:     "1.0",
		Arch:        arch,
		IndexArch:   indexArch,
		Maintainer:  "Nobody <nobody@example.org>",
		Description: "test package",
		Section:     "misc",
		Priority:    "optional",
		Filename:    "pool/main/" + name + "_1.0_" + arch + ".deb",
		Size:        "1234",
		SHA256:      "cafe",
		FilenameUrl: "http://example.org/debian",
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// Regression test: the per-architecture buffer used to be shared, so every
// architecture after the first inherited the previous one's packages.
func TestGenerateDistsKeepsArchitecturesSeparate(t *testing.T) {
	config := testConfig(t, "amd64", "i386")

	selected := map[string][]*PackageDeb{
		"amd64": {pkg("only-amd64", "amd64", "amd64"), pkg("shared", "all", "amd64")},
		"i386":  {pkg("only-i386", "i386", "i386"), pkg("shared", "all", "i386")},
	}

	result, err := generateDists(config, selected)
	if err != nil {
		t.Fatalf("generateDists: %v", err)
	}

	base := filepath.Join(config.Destination.Path, "dists", DestinationDistsName, DestinationComponent)
	amd64 := readFile(t, filepath.Join(base, "binary-amd64", "Packages"))
	i386 := readFile(t, filepath.Join(base, "binary-i386", "Packages"))

	if strings.Contains(i386, "only-amd64") {
		t.Error("binary-i386/Packages leaked amd64 packages")
	}
	if strings.Contains(amd64, "only-i386") {
		t.Error("binary-amd64/Packages leaked i386 packages")
	}
	if !strings.Contains(amd64, "Package: only-amd64") {
		t.Error("binary-amd64/Packages is missing its own package")
	}
	if !strings.Contains(i386, "Package: only-i386") {
		t.Error("binary-i386/Packages is missing its own package")
	}

	// An Architecture:all package keeps "all" in both indexes.
	if !strings.Contains(amd64, "Architecture: all") || !strings.Contains(i386, "Architecture: all") {
		t.Error("Architecture: all was rewritten")
	}

	if len(result.Architectures) != 2 {
		t.Errorf("Architectures = %v, want 2", result.Architectures)
	}
	// Packages + Packages.gz + Packages.xz for each of the two architectures.
	if len(result.Entries) != 6 {
		t.Errorf("Entries = %d, want 6", len(result.Entries))
	}
	for _, entry := range result.Entries {
		if !fileExists(entry.Abs) {
			t.Errorf("missing %s", entry.Abs)
		}
		if strings.Contains(entry.Rel, `\`) {
			t.Errorf("Release path must use forward slashes: %s", entry.Rel)
		}
	}
}

func TestGenerateDistsEmitsStandardFieldsOnly(t *testing.T) {
	config := testConfig(t, "amd64")

	p := pkg("nano", "amd64", "amd64")
	p.Pre_Depends = "dpkg (>= 1.15)"
	p.Homepage = ""

	if _, err := generateDists(config, map[string][]*PackageDeb{"amd64": {p}}); err != nil {
		t.Fatalf("generateDists: %v", err)
	}

	content := readFile(t, filepath.Join(config.Destination.Path,
		"dists", DestinationDistsName, DestinationComponent, "binary-amd64", "Packages"))

	if strings.Contains(content, "#FilenameUrl") {
		t.Error("the non-standard #FilenameUrl field is still published")
	}
	if !strings.Contains(content, "Pre-Depends: dpkg (>= 1.15)") {
		t.Error("Pre-Depends is not written to the index")
	}
	if strings.Contains(content, "Homepage:") {
		t.Error("empty fields must not be emitted")
	}
	if !strings.HasSuffix(content, "\n\n") {
		t.Error("stanza must be terminated by a blank line")
	}
}

func TestGenerateRelease(t *testing.T) {
	config := testConfig(t, "amd64")

	result, err := generateDists(config, map[string][]*PackageDeb{
		"amd64": {pkg("nano", "amd64", "amd64")},
	})
	if err != nil {
		t.Fatalf("generateDists: %v", err)
	}

	if err := generateRelease(result); err != nil {
		t.Fatalf("generateRelease: %v", err)
	}

	release := readFile(t, filepath.Join(result.Root, "Release"))

	for _, want := range []string{
		"Codename: " + DestinationDistsName,
		"Architectures: amd64",
		"Components: " + DestinationComponent,
		"Date: ",
		"MD5Sum:",
		"SHA256:",
		"main/binary-amd64/Packages",
		"main/binary-amd64/Packages.gz",
		"main/binary-amd64/Packages.xz",
	} {
		if !strings.Contains(release, want) {
			t.Errorf("Release is missing %q\n---\n%s", want, release)
		}
	}

	// Checksum lines must be indented by exactly one space.
	for _, line := range strings.Split(release, "\n") {
		if strings.Contains(line, "binary-amd64/Packages") && !strings.HasPrefix(line, " ") {
			t.Errorf("checksum line is not indented: %q", line)
		}
	}
}

func TestManifestRoundTrip(t *testing.T) {
	config := testConfig(t, "amd64", "i386")

	shared := pkg("shared", "all", "amd64")
	sharedI386 := pkg("shared", "all", "i386")
	// Architecture:all maps to a single file in the pool.
	sharedI386.Filename = shared.Filename

	selected := map[string][]*PackageDeb{
		"amd64": {pkg("only-amd64", "amd64", "amd64"), shared},
		"i386":  {pkg("only-i386", "i386", "i386"), sharedI386},
	}

	if err := writeManifest(config, selected); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}

	entries, err := readManifest(manifestPath(config))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (the shared pool file must appear once)", len(entries))
	}

	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.PoolPath] {
			t.Errorf("duplicate pool path: %s", entry.PoolPath)
		}
		seen[entry.PoolPath] = true

		if entry.BaseURL != "http://example.org/debian" {
			t.Errorf("BaseURL = %q", entry.BaseURL)
		}
		if entry.Size != 1234 {
			t.Errorf("Size = %d, want 1234", entry.Size)
		}
		if entry.SHA256 != "cafe" {
			t.Errorf("SHA256 = %q", entry.SHA256)
		}
	}
}

func TestReadManifestRejectsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.tsv")

	if err := os.WriteFile(path, []byte(ManifestHeader+"only\ttwo\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := readManifest(path); err == nil {
		t.Error("expected an error for a malformed line")
	}
}

func TestParseSources(t *testing.T) {
	targets := parseSources([]string{
		"http://deb.debian.org/debian bookworm main contrib",
		"garbage",
	}, []string{"amd64", "i386"})

	// 2 components x 2 architectures; the malformed line is skipped.
	if len(targets) != 4 {
		t.Fatalf("got %d targets, want 4", len(targets))
	}

	got := targets[0].sourceDir()
	want := "http://deb.debian.org/debian/dists/bookworm/main/binary-amd64"
	if got != want {
		t.Errorf("sourceDir() = %q, want %q", got, want)
	}
}

func TestIndexTargetTrimsTrailingSlash(t *testing.T) {
	target := indexTarget{BaseURL: "http://example.org/debian/", Dist: "bookworm", Component: "main", Arch: "amd64"}

	if strings.Contains(target.sourceDir(), "//dists") {
		t.Errorf("sourceDir() = %q", target.sourceDir())
	}
}
