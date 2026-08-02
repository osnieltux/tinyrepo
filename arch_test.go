package main

import (
	"archive/tar"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestArchDependencyName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"glibc", "glibc"},
		{"glibc>=2.34", "glibc"},
		{"python<3.12", "python"},
		{"ncurses<=6.6", "ncurses"},
		{"gcc>13", "gcc"},
		{"libreadline.so=8-64", "libreadline.so"},
		{"lib32-glibc=2.44", "lib32-glibc"},
		{"  spaced  ", "spaced"},
		{"", ""},
		{">=1.0", ""},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			if got := archDependencyName(tc.input); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestArchDependencyClauses(t *testing.T) {
	// pacman has no alternatives, so every clause holds exactly one name.
	got := archDependencyClauses([]string{"glibc>=2.34", "", "libreadline.so=8-64"})

	if len(got) != 2 {
		t.Fatalf("got %v, want 2 clauses (the empty entry must be dropped)", got)
	}
	for _, clause := range got {
		if len(clause) != 1 {
			t.Errorf("clause %v has %d alternatives, want 1", clause, len(clause))
		}
	}
	if got[0][0] != "glibc" || got[1][0] != "libreadline.so" {
		t.Errorf("got %v", got)
	}
}

func TestIsDescKey(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"%NAME%", true},
		{"%SHA256SUM%", true},
		{"%OPTDEPENDS%", true},
		{"%FOO_BAR%", true},
		{"bash", false},
		{"", false},
		{"%%", false},
		{"50% faster", false},
		{"%lowercase%", false},
		{"%with space%", false},
		{"100%", false},
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			if got := isDescKey(tc.line); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

const realDesc = `%FILENAME%
bash-5.3.15-1-x86_64.pkg.tar.zst

%NAME%
bash

%VERSION%
5.3.15-1

%DESC%
The GNU Bourne Again shell

%CSIZE%
2002014

%SHA256SUM%
2c14ce3c72790ec6b344267b39f04f921bbb34f0c1399460e71492547f6b6c65

%ARCH%
x86_64

%PROVIDES%
sh

%DEPENDS%
readline
libreadline.so=8-64
glibc
ncurses

%OPTDEPENDS%
bash-completion: for tab completion
`

func TestParseDesc(t *testing.T) {
	fields := parseDesc([]byte(realDesc))

	if got := descFirst(fields, "NAME"); got != "bash" {
		t.Errorf("NAME = %q", got)
	}
	if got := descFirst(fields, "FILENAME"); got != "bash-5.3.15-1-x86_64.pkg.tar.zst" {
		t.Errorf("FILENAME = %q", got)
	}
	if got := descFirst(fields, "DESC"); got != "The GNU Bourne Again shell" {
		t.Errorf("DESC = %q", got)
	}

	// A multi-valued field keeps every line.
	want := []string{"readline", "libreadline.so=8-64", "glibc", "ncurses"}
	if got := fields["DEPENDS"]; !slices.Equal(got, want) {
		t.Errorf("DEPENDS = %v, want %v", got, want)
	}
	if got := fields["PROVIDES"]; !slices.Equal(got, []string{"sh"}) {
		t.Errorf("PROVIDES = %v", got)
	}
	if _, ok := fields["MISSING"]; ok {
		t.Error("a field that is not present must not appear")
	}
}

func TestParseDescIgnoresValuesThatLookLikeKeys(t *testing.T) {
	fields := parseDesc([]byte("%DESC%\n50% faster\n100%\n"))

	if got := fields["DESC"]; !slices.Equal(got, []string{"50% faster", "100%"}) {
		t.Errorf("DESC = %v", got)
	}
}

func TestArchMemberDir(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"bash-5.3.15-1/desc", "bash-5.3.15-1"},
		{"bash-5.3.15-1/files", "bash-5.3.15-1"},
		{"bash-5.3.15-1/", "bash-5.3.15-1"},
		{"./bash-5.3.15-1/desc", "bash-5.3.15-1"},
		{"", ""},
		{"/", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := archMemberDir(tc.name); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseArchSourcesExpandsTemplates(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			"arch",
			"https://geo.mirror.pkgbuild.com/$repo/os/$arch",
			"https://geo.mirror.pkgbuild.com/core/os/x86_64",
		},
		{
			// Manjaro puts the branch first and has no os/ level; the same
			// placeholders cover it.
			"manjaro",
			"https://mirror.alpix.eu/manjaro/stable/$repo/$arch",
			"https://mirror.alpix.eu/manjaro/stable/core/x86_64",
		},
		{
			"trailing slash",
			"https://mirror.example/$repo/os/$arch/",
			"https://mirror.example/core/os/x86_64",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			targets := parseArchSources([]string{tc.template + " core"}, []string{"x86_64"})
			if len(targets) != 1 {
				t.Fatalf("got %d targets, want 1", len(targets))
			}

			if got := targets[0].baseURL(); got != tc.want {
				t.Errorf("baseURL() = %q, want %q", got, tc.want)
			}
			if got := targets[0].dbURL(); got != tc.want+"/core.db" {
				t.Errorf("dbURL() = %q", got)
			}
			if got := targets[0].filesURL(); got != tc.want+"/core.files" {
				t.Errorf("filesURL() = %q", got)
			}
		})
	}
}

func TestParseArchSourcesFanOut(t *testing.T) {
	targets := parseArchSources([]string{
		"https://mirror.example/$repo/os/$arch core extra multilib",
		"garbage-with-no-repo",
	}, []string{"x86_64"})

	// Three repositories x one architecture; the malformed line is skipped.
	if len(targets) != 3 {
		t.Fatalf("got %d targets, want 3", len(targets))
	}

	var repos []string
	for _, target := range targets {
		repos = append(repos, target.Repo)
	}
	if want := []string{"core", "extra", "multilib"}; !slices.Equal(repos, want) {
		t.Errorf("repos = %v, want %v", repos, want)
	}
}

func TestArchTargetCachePaths(t *testing.T) {
	target := archTarget{Template: "https://mirror.example/$repo/os/$arch", Repo: "core", Arch: "x86_64"}

	db := target.cachedDB("/tmp/repo")
	if want := filepath.Join("/tmp/repo", ArchCacheName, "core", "x86_64", "core.db"); db != want {
		t.Errorf("cachedDB() = %q, want %q", db, want)
	}
	if files := target.cachedFiles("/tmp/repo"); !strings.HasSuffix(files, "core.files") {
		t.Errorf("cachedFiles() = %q", files)
	}
}

// archTestEntry builds an entry whose members look like a real database's.
func archTestEntry(dir, desc, files string) *archEntry {
	entry := &archEntry{Dir: dir}

	entry.Members = append(entry.Members, archMember{
		Header: &tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0755},
	})
	entry.Members = append(entry.Members, archMember{
		Header: &tar.Header{Name: dir + "/desc", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(desc))},
		Data:   []byte(desc),
	})
	if files != "" {
		entry.Members = append(entry.Members, archMember{
			Header: &tar.Header{Name: dir + "/files", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(files))},
			Data:   []byte(files),
		})
	}

	applyDesc(entry, parseDesc([]byte(desc)))
	return entry
}

func TestArchDBRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tinyrepo.db")

	written := []*archEntry{
		archTestEntry("bash-5.3.15-1", realDesc, "%FILES%\nusr/bin/bash\n"),
		archTestEntry("nano-9.1-1", "%FILENAME%\nnano-9.1-1-x86_64.pkg.tar.zst\n\n%NAME%\nnano\n\n%CSIZE%\n700\n\n%ARCH%\nx86_64\n", ""),
	}

	if err := writeArchDB(dbPath, written); err != nil {
		t.Fatalf("writeArchDB: %v", err)
	}

	// pacman fetches "<repo>.db"; the .tar.gz name is written for humans.
	for _, name := range []string{dbPath, dbPath + ".tar.gz"} {
		if !fileExists(name) {
			t.Fatalf("%s was not written", name)
		}
	}
	if readFile(t, dbPath) != readFile(t, dbPath+".tar.gz") {
		t.Error("the two names must hold identical bytes")
	}

	read, err := readArchDB(dbPath, "core", "https://mirror.example/core/os/x86_64")
	if err != nil {
		t.Fatalf("readArchDB: %v", err)
	}

	if len(read) != 2 {
		t.Fatalf("got %d entries, want 2", len(read))
	}

	bash := read[0]
	if bash.Name != "bash" {
		t.Errorf("Name = %q", bash.Name)
	}
	if bash.Filename != "bash-5.3.15-1-x86_64.pkg.tar.zst" {
		t.Errorf("Filename = %q", bash.Filename)
	}
	if bash.Size != 2002014 {
		t.Errorf("Size = %d", bash.Size)
	}
	if bash.SHA256 == "" {
		t.Error("SHA256 was not parsed")
	}
	if bash.Repo != "core" || bash.BaseURL != "https://mirror.example/core/os/x86_64" {
		t.Errorf("Repo/BaseURL = %q/%q", bash.Repo, bash.BaseURL)
	}
	if !slices.Equal(archProvidedNames(bash.Provides), []string{"sh"}) {
		t.Errorf("Provides = %v", bash.Provides)
	}

	// The desc member must survive the round trip untouched: that is what
	// keeps %PGPSIG% and any field tinyrepo does not know about.
	var desc string
	for _, member := range bash.Members {
		if strings.HasSuffix(member.Header.Name, "/desc") {
			desc = string(member.Data)
		}
	}
	if desc != realDesc {
		t.Errorf("the desc member changed across the round trip:\n%q", desc)
	}
}

func TestArchDBOutputIsDeterministic(t *testing.T) {
	dir := t.TempDir()

	entries := []*archEntry{
		archTestEntry("zzz-1-1", "%NAME%\nzzz\n", ""),
		archTestEntry("aaa-1-1", "%NAME%\naaa\n", ""),
	}

	first := filepath.Join(dir, "first.db")
	second := filepath.Join(dir, "second.db")

	if err := writeArchDB(first, entries); err != nil {
		t.Fatal(err)
	}
	// Reversed input must produce the same file: entries are sorted by name.
	slices.Reverse(entries)
	if err := writeArchDB(second, entries); err != nil {
		t.Fatal(err)
	}

	if readFile(t, first) != readFile(t, second) {
		t.Error("the generated database depends on the order of the input")
	}
}

func TestArchIndexCatalog(t *testing.T) {
	index := &archIndex{ByName: map[string]*archEntry{}}

	for _, entry := range []*archEntry{
		{Name: "bash", Depends: []string{"readline", "libreadline.so=8-64"}, Provides: []string{"sh"}},
		{Name: "readline", Provides: []string{"libreadline.so=8-64"}},
		{Name: "plasma-desktop", Groups: []string{"plasma"}},
	} {
		index.ByName[entry.Name] = entry
	}

	selected, unresolved := index.catalog().resolveNames([]string{"bash", "plasma"})

	want := []string{"bash", "plasma-desktop", "readline"}
	if !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none: the soname is provided by readline", unresolved)
	}
}

func TestValidateConfigBackendType(t *testing.T) {
	tests := []struct {
		backendType string
		wantErr     bool
	}{
		{BackendDebian, false},
		{BackendArch, false},
		{"", true},
		{"redhat", true},
		{"Debian", true}, // the value is matched exactly
	}

	for _, tc := range tests {
		t.Run(tc.backendType, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Server.Type = tc.backendType
			cfg.Server.Source = []string{"https://mirror.example/$repo/os/$arch core"}
			cfg.Destination.Path = "/tmp/repo"

			if err := validateConfig(&cfg); (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateConfigRejectsAnyArchitecture(t *testing.T) {
	// "any" is to pacman what "all" is to Debian: not a real index.
	for _, arch := range []string{"all", "any"} {
		t.Run(arch, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Server.Source = []string{"http://mirror.example/debian bookworm main"}
			cfg.Destination.Path = "/tmp/repo"
			cfg.Destination.Arch = []string{arch}

			if err := validateConfig(&cfg); err == nil {
				t.Errorf("validateConfig accepted arch = %q", arch)
			}
		})
	}
}

func TestNewBackend(t *testing.T) {
	for _, name := range []string{BackendDebian, BackendArch} {
		backend, err := newBackend(name)
		if err != nil {
			t.Errorf("newBackend(%q): %v", name, err)
		}
		if backend == nil {
			t.Errorf("newBackend(%q) returned nil", name)
		}
	}

	if _, err := newBackend("nope"); err == nil {
		t.Error("newBackend accepted an unknown type")
	}
}
