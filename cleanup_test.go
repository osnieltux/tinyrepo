package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// seedRepo writes a destination tree from "path -> contents" and returns it.
func seedRepo(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, contents := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// everything a generated repository holds besides the packages themselves.
var repositoryFurniture = map[string]string{
	"dists/tinyrepo/Release":                       "Origin: tinyrepo",
	"dists/tinyrepo/main/binary-amd64/Packages":    "Package: nano",
	"dists/tinyrepo/main/binary-amd64/Packages.gz": "gz",
	"dists/tinyrepo/main/binary-amd64/Packages.xz": "xz",
	"tinyrepo.db":            "db",
	"tinyrepo.db.tar.gz":     "db",
	"tinyrepo.files":         "files",
	"tinyrepo.files.tar.gz":  "files",
	".tinyrepo/manifest.tsv": "manifest",
	"dists_cache/bookworm/main/binary-amd64/Packages": "cached",
	"db_cache/core/x86_64/core.db":                    "cached",
}

// -cl must never be able to reach the repository's own files, whatever the keep
// set says. This is the safety property the whole command rests on.
func TestCleanupCandidatesNeverIncludeRepositoryFiles(t *testing.T) {
	files := map[string]string{
		"pool/main/n/nano/nano_7.2_amd64.deb": "deb",
		"nano-8.6-1-x86_64.pkg.tar.zst":       "pkg",
	}
	for name, contents := range repositoryFurniture {
		files[name] = contents
	}

	root := seedRepo(t, files)

	candidates, err := cleanupCandidates(root)
	if err != nil {
		t.Fatalf("cleanupCandidates: %v", err)
	}

	for name := range repositoryFurniture {
		if slices.Contains(candidates, name) {
			t.Errorf("%s is a deletion candidate, but it is part of the repository", name)
		}
	}

	// The two real packages must be candidates, or -cl would do nothing.
	for _, want := range []string{"pool/main/n/nano/nano_7.2_amd64.deb", "nano-8.6-1-x86_64.pkg.tar.zst"} {
		if !slices.Contains(candidates, want) {
			t.Errorf("%s is not a deletion candidate, so it could never be cleaned", want)
		}
	}
}

func TestCleanupCandidates(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name:  "debian packages live under pool",
			files: map[string]string{"pool/main/n/nano/nano_7.2_amd64.deb": "x"},
			want:  []string{"pool/main/n/nano/nano_7.2_amd64.deb"},
		},
		{
			name: "pacman packages and their signatures are flat",
			files: map[string]string{
				"nano-8.6-1-x86_64.pkg.tar.zst":     "x",
				"nano-8.6-1-x86_64.pkg.tar.zst.sig": "x",
			},
			want: []string{"nano-8.6-1-x86_64.pkg.tar.zst", "nano-8.6-1-x86_64.pkg.tar.zst.sig"},
		},
		{
			name:  "the pacman databases are not packages",
			files: map[string]string{"tinyrepo.db.tar.gz": "x", "tinyrepo.files.tar.gz": "x"},
			want:  nil,
		},
		{
			name:  "half-written downloads are candidates wherever they are",
			files: map[string]string{"nano-8.6-1-x86_64.pkg.tar.zst" + TempSuffix: "x"},
			want:  []string{"nano-8.6-1-x86_64.pkg.tar.zst" + TempSuffix},
		},
		{
			name:  "an empty repository has nothing to clean",
			files: map[string]string{},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cleanupCandidates(seedRepo(t, tc.files))
			if err != nil {
				t.Fatalf("cleanupCandidates: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("candidates = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCleanupPlan(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		keep       []string
		want       []string
	}{
		{
			name:       "declared packages stay",
			candidates: []string{"pool/a.deb", "pool/b.deb"},
			keep:       []string{"pool/a.deb"},
			want:       []string{"pool/b.deb"},
		},
		{
			name:       "a signature follows its package",
			candidates: []string{"a.pkg.tar.zst", "a.pkg.tar.zst.sig", "b.pkg.tar.zst", "b.pkg.tar.zst.sig"},
			keep:       []string{"a.pkg.tar.zst"},
			want:       []string{"b.pkg.tar.zst", "b.pkg.tar.zst.sig"},
		},
		{
			name:       "a half-written file always goes, even if the package is kept",
			candidates: []string{"pool/a.deb", "pool/a.deb" + TempSuffix},
			keep:       []string{"pool/a.deb", "pool/a.deb" + TempSuffix},
			want:       []string{"pool/a.deb" + TempSuffix},
		},
		{
			name:       "nothing to do",
			candidates: []string{"pool/a.deb"},
			keep:       []string{"pool/a.deb"},
			want:       nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keep := map[string]bool{}
			for _, path := range tc.keep {
				keep[path] = true
			}

			if got := cleanupPlan(tc.candidates, keep); !slices.Equal(got, tc.want) {
				t.Errorf("plan = %v, want %v", got, tc.want)
			}
		})
	}
}

// cleanBackend is a Backend that returns a fixed catalog, so cleanRepo can be
// exercised without a cache on disk.
type cleanBackend struct{ cat *repoCatalog }

func (cleanBackend) FetchIndexes(*Config) error { return nil }
func (cleanBackend) Build(*Config) error        { return nil }

func (b cleanBackend) Catalog(*Config) (*repoCatalog, error) { return b.cat, nil }

// The headline behaviour: what config.toml asks for stays, what arrived on
// demand goes.
func TestCleanRepoKeepsDeclaredRemovesTheRest(t *testing.T) {
	files := map[string]string{
		"pool/main/n/nano/nano_7.2_amd64.deb":  "nano",
		"pool/main/l/libc/libc_2.36_amd64.deb": "libc",
		// Fetched on demand, never declared.
		"pool/main/v/vim/vim_9_amd64.deb": "vim",
		// Superseded upstream, so it is not in the catalog at all.
		"pool/main/n/nano/nano_7.0_amd64.deb": "old nano",
	}
	for name, contents := range repositoryFurniture {
		files[name] = contents
	}

	root := seedRepo(t, files)

	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {
			{name: "nano", path: "pool/main/n/nano/nano_7.2_amd64.deb", deps: []string{"libc"}},
			{name: "libc", path: "pool/main/l/libc/libc_2.36_amd64.deb"},
			{name: "vim", path: "pool/main/v/vim/vim_9_amd64.deb"},
		},
	})

	config := defaultConfig()
	config.Destination.Path = root
	config.Destination.Packages = []string{"nano"}

	if err := cleanRepo(&config, cleanBackend{cat: cat}); err != nil {
		t.Fatalf("cleanRepo: %v", err)
	}

	// nano is declared and libc is its dependency, so both stay.
	for _, keep := range []string{
		"pool/main/n/nano/nano_7.2_amd64.deb",
		"pool/main/l/libc/libc_2.36_amd64.deb",
	} {
		if !fileExists(filepath.Join(root, filepath.FromSlash(keep))) {
			t.Errorf("%s was deleted, but config.toml asks for it", keep)
		}
	}

	// vim was never declared, and the old nano is not even in the catalog.
	for _, gone := range []string{
		"pool/main/v/vim/vim_9_amd64.deb",
		"pool/main/n/nano/nano_7.0_amd64.deb",
	} {
		if fileExists(filepath.Join(root, filepath.FromSlash(gone))) {
			t.Errorf("%s survived, but nothing in config.toml asks for it", gone)
		}
	}

	// And nothing of the repository itself was touched.
	for name := range repositoryFurniture {
		if !fileExists(filepath.Join(root, filepath.FromSlash(name))) {
			t.Errorf("%s was deleted, but it is part of the repository", name)
		}
	}
}

// A declared list that resolves to nothing means a stale cache or a typo.
// Reading that as "delete everything" would be indefensible.
func TestCleanRepoRefusesWhenNothingResolves(t *testing.T) {
	root := seedRepo(t, map[string]string{"pool/main/n/nano/nano_7.2_amd64.deb": "nano"})

	config := defaultConfig()
	config.Destination.Path = root
	config.Destination.Packages = []string{"a-package-the-catalog-does-not-have"}

	empty := buildRepoCatalog(map[string][]catalogPkg{"amd64": {}})

	if err := cleanRepo(&config, cleanBackend{cat: empty}); err == nil {
		t.Error("cleanRepo went ahead with an empty keep set")
	}
	if !fileExists(filepath.Join(root, "pool/main/n/nano/nano_7.2_amd64.deb")) {
		t.Error("cleanRepo deleted a package after refusing to run")
	}
}

// The pure proxy case: nothing is declared, so nothing is kept. That is
// unambiguous, unlike the case above.
func TestCleanRepoWithNoDeclaredPackagesRemovesEverything(t *testing.T) {
	root := seedRepo(t, map[string]string{"pool/main/v/vim/vim_9_amd64.deb": "vim"})

	config := defaultConfig()
	config.Destination.Path = root
	config.Destination.Packages = nil

	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {{name: "vim", path: "pool/main/v/vim/vim_9_amd64.deb"}},
	})

	if err := cleanRepo(&config, cleanBackend{cat: cat}); err != nil {
		t.Fatalf("cleanRepo: %v", err)
	}
	if fileExists(filepath.Join(root, "pool/main/v/vim/vim_9_amd64.deb")) {
		t.Error("a package survived even though config.toml declares none")
	}
}

func TestCleanRepoRemovesStaleTempFiles(t *testing.T) {
	partial := "pool/main/n/nano/nano_7.2_amd64.deb" + TempSuffix

	root := seedRepo(t, map[string]string{
		"pool/main/n/nano/nano_7.2_amd64.deb": "nano",
		partial:                               "half a package",
	})

	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {{name: "nano", path: "pool/main/n/nano/nano_7.2_amd64.deb"}},
	})

	config := defaultConfig()
	config.Destination.Path = root
	config.Destination.Packages = []string{"nano"}

	if err := cleanRepo(&config, cleanBackend{cat: cat}); err != nil {
		t.Fatalf("cleanRepo: %v", err)
	}

	if fileExists(filepath.Join(root, filepath.FromSlash(partial))) {
		t.Error("an interrupted download was left behind")
	}
	if !fileExists(filepath.Join(root, "pool/main/n/nano/nano_7.2_amd64.deb")) {
		t.Error("the finished package was deleted along with the temporary one")
	}
}

func TestCleanRepoPrunesEmptyDirectories(t *testing.T) {
	root := seedRepo(t, map[string]string{"pool/main/v/vim/vim_9_amd64.deb": "vim"})

	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {{name: "vim", path: "pool/main/v/vim/vim_9_amd64.deb"}},
	})

	config := defaultConfig()
	config.Destination.Path = root
	config.Destination.Packages = nil

	if err := cleanRepo(&config, cleanBackend{cat: cat}); err != nil {
		t.Fatalf("cleanRepo: %v", err)
	}

	if fileExists(filepath.Join(root, "pool/main/v/vim")) {
		t.Error("pool/main/v/vim survived as an empty directory")
	}
}

// A failure to read the catalog has to say what to do about it, because the
// answer is always "run -di first".
func TestCleanRepoReportsCatalogFailure(t *testing.T) {
	config := defaultConfig()
	config.Destination.Path = t.TempDir()
	config.Server.Source = []string{"http://example.org/debian bookworm main"}

	err := cleanRepo(&config, debianBackend{})
	if err == nil {
		t.Fatal("cleanRepo succeeded with no index cache")
	}
	if !strings.Contains(err.Error(), _t("err catalog")) {
		t.Errorf("error = %q, want it to mention the catalog", err)
	}
}
