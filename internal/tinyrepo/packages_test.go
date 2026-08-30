package tinyrepo

import (
	"slices"
	"strings"
	"testing"
)

func TestDependencyReader(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  [][]string
	}{
		{"empty", "", nil},
		{"single", "libc6", [][]string{{"libc6"}}},
		{"version constraint", "libc6 (>= 2.34)", [][]string{{"libc6"}}},
		{"arch qualifier", "perl:any", [][]string{{"perl"}}},
		{"alternatives", "perl | perl-base", [][]string{{"perl", "perl-base"}}},
		{
			"several clauses",
			"libc6 (>= 2.34), libtinfo6 (>= 6), zlib1g (>= 1:1.1.4)",
			[][]string{{"libc6"}, {"libtinfo6"}, {"zlib1g"}},
		},
		{
			"mixed",
			"debconf (>= 0.5) | debconf-2.0, libc6:amd64 (>= 2.34)",
			[][]string{{"debconf", "debconf-2.0"}, {"libc6"}},
		},
		{"arch list", "libc6 [amd64 i386]", [][]string{{"libc6"}}},
		{"build profile", "dpkg <!nocheck>", [][]string{{"dpkg"}}},
		{"trailing comma", "libc6,", [][]string{{"libc6"}}},
		{"only separators", " , | , ", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dependencyReader(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if !slices.Equal(got[i], tc.want[i]) {
					t.Errorf("clause %d: got %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestIndexArchFromPath(t *testing.T) {
	tests := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{"/tmp/repo/dists_cache/bookworm/main/binary-amd64/Packages", "amd64", false},
		{"/tmp/repo/dists_cache/bookworm/main/binary-arm64/Packages", "arm64", false},
		{"/tmp/repo/dists_cache/bookworm/main/source/Sources", "", true},
		{"/tmp/repo/dists_cache/bookworm/main/binary-/Packages", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, err := indexArchFromPath(tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

const twoStanzas = `Package: nano
Version: 7.2-1
Architecture: amd64
Installed-Size: 2400
Depends: libc6 (>= 2.34), libncursesw6 (>= 6)
Pre-Depends: dpkg (>= 1.15)
Filename: pool/main/n/nano/nano_7.2-1_amd64.deb
Size: 663180
SHA256: aaaa
Description: small, friendly text editor
 A long continuation line that must not start a new stanza.
Section: editors
Priority: important

Package: fonts-dejavu
Version: 2.37-6
Architecture: all
Filename: pool/main/f/fonts-dejavu/fonts-dejavu_2.37-6_all.deb
Size: 1000
SHA256: bbbb
Section: fonts
Priority: optional`

func TestParsePackages(t *testing.T) {
	packages, err := parsePackages(strings.NewReader(twoStanzas), "amd64", "http://example.org/debian")
	if err != nil {
		t.Fatalf("parsePackages: %v", err)
	}

	// The input deliberately has no trailing blank line: the last stanza must
	// still be flushed.
	if len(packages) != 2 {
		t.Fatalf("got %d packages, want 2", len(packages))
	}

	nano := packages[0]
	if nano.Package != "nano" {
		t.Errorf("Package = %q", nano.Package)
	}
	if nano.Version != "7.2-1" {
		t.Errorf("Version = %q", nano.Version)
	}
	if nano.Pre_Depends != "dpkg (>= 1.15)" {
		t.Errorf("Pre-Depends = %q", nano.Pre_Depends)
	}
	if nano.Description != "small, friendly text editor" {
		t.Errorf("Description = %q", nano.Description)
	}
	if nano.FilenameUrl != "http://example.org/debian" {
		t.Errorf("FilenameUrl = %q", nano.FilenameUrl)
	}
	if nano.IndexArch != "amd64" {
		t.Errorf("IndexArch = %q", nano.IndexArch)
	}

	// An Architecture:all package keeps "all", but belongs to the amd64 index.
	fonts := packages[1]
	if fonts.Arch != "all" {
		t.Errorf("Arch = %q, want %q", fonts.Arch, "all")
	}
	if fonts.IndexArch != "amd64" {
		t.Errorf("IndexArch = %q, want %q", fonts.IndexArch, "amd64")
	}
}

func TestParsePackagesFallsBackToIndexArch(t *testing.T) {
	input := "Package: foo\nVersion: 1\n\n"

	packages, err := parsePackages(strings.NewReader(input), "i386", "")
	if err != nil {
		t.Fatalf("parsePackages: %v", err)
	}
	if len(packages) != 1 {
		t.Fatalf("got %d packages, want 1", len(packages))
	}
	if packages[0].Arch != "i386" {
		t.Errorf("Arch = %q, want %q", packages[0].Arch, "i386")
	}
}

func TestParsePackagesIgnoresBlankStanzas(t *testing.T) {
	input := "\n\n\nPackage: foo\nVersion: 1\n\n\n\n"

	packages, err := parsePackages(strings.NewReader(input), "amd64", "")
	if err != nil {
		t.Fatalf("parsePackages: %v", err)
	}
	if len(packages) != 1 {
		t.Fatalf("got %d packages, want 1", len(packages))
	}
}

func testIndex(t *testing.T) map[string]*PackageDeb {
	t.Helper()
	index := map[string]*PackageDeb{}

	add := func(name, depends, preDepends string) {
		index[name] = &PackageDeb{
			Package:     name,
			Arch:        "amd64",
			IndexArch:   "amd64",
			Depends:     depends,
			Pre_Depends: preDepends,
			Filename:    "pool/main/" + name + ".deb",
		}
	}

	add("nano", "libc6, libncursesw6", "dpkg")
	add("libc6", "", "")
	add("libncursesw6", "libtinfo6", "")
	add("libtinfo6", "libc6", "") // cycle back to an already-visited package
	add("dpkg", "libc6", "")
	add("wget", "libc6 | libc6-udeb, libssl3", "")
	add("libssl3", "", "")

	return index
}

func TestResolveDependenciesTransitive(t *testing.T) {
	packages, unresolved := resolveDependencies(testIndex(t), []string{"nano"})

	var names []string
	for _, pkg := range packages {
		names = append(names, pkg.Package)
	}

	// dpkg only arrives through Pre-Depends; libtinfo6 only transitively.
	want := []string{"dpkg", "libc6", "libncursesw6", "libtinfo6", "nano"}
	if !slices.Equal(names, want) {
		t.Errorf("got %v, want %v", names, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

func TestResolveDependenciesDeduplicates(t *testing.T) {
	// nano appears twice as a seed and is also reachable from itself.
	packages, _ := resolveDependencies(testIndex(t), []string{"nano", "nano", "libc6"})

	seen := map[string]int{}
	for _, pkg := range packages {
		seen[pkg.Package]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("%s appears %d times, want 1", name, count)
		}
	}
}

func TestResolveDependenciesPicksAvailableAlternative(t *testing.T) {
	index := testIndex(t)
	delete(index, "libc6")
	index["libc6-udeb"] = &PackageDeb{Package: "libc6-udeb", Arch: "amd64", IndexArch: "amd64"}

	packages, _ := resolveDependencies(index, []string{"wget"})

	var names []string
	for _, pkg := range packages {
		names = append(names, pkg.Package)
	}
	// "libc6 | libc6-udeb": libc6 is gone, so the second alternative is taken.
	if !slices.Contains(names, "libc6-udeb") {
		t.Errorf("got %v, want it to contain libc6-udeb", names)
	}
}

func TestResolveDependenciesReportsMissing(t *testing.T) {
	index := testIndex(t)
	delete(index, "libssl3")

	_, unresolved := resolveDependencies(index, []string{"wget", "does-not-exist"})

	if !slices.Equal(unresolved, []string{"does-not-exist", "libssl3"}) {
		t.Errorf("unresolved = %v", unresolved)
	}
}

func TestResolveDependenciesIsDeterministic(t *testing.T) {
	index := testIndex(t)

	first, _ := resolveDependencies(index, []string{"nano", "wget"})
	for range 20 {
		next, _ := resolveDependencies(index, []string{"nano", "wget"})
		if len(next) != len(first) {
			t.Fatalf("length changed: %d != %d", len(next), len(first))
		}
		for i := range first {
			if next[i].Package != first[i].Package {
				t.Fatalf("order changed at %d: %s != %s", i, next[i].Package, first[i].Package)
			}
		}
	}
}
