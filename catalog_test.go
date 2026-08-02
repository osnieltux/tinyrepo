package main

import (
	"maps"
	"slices"
	"strconv"
	"sync"
	"testing"
)

// catalogPkg describes one package for the fixtures below.
type catalogPkg struct {
	name string
	path string
	deps []string
}

// buildRepoCatalog wires packages into a catalog, one view per architecture.
func buildRepoCatalog(arches map[string][]catalogPkg) *repoCatalog {
	cat := newRepoCatalog()

	for _, arch := range slices.Sorted(maps.Keys(arches)) {
		view := newArchCatalogView(arch, newCatalog())

		for _, p := range arches[arch] {
			var clauses [][]string
			for _, dependency := range p.deps {
				clauses = append(clauses, []string{dependency})
			}
			view.Deps.addPackage(p.name, clauses, nil, nil)

			cat.add(view, p.name, manifestEntry{
				BaseURL:  "http://mirror.test",
				PoolPath: p.path,
				Size:     int64(len(p.path)),
				SHA256:   "sha-" + p.name,
			})
		}
		cat.Arches = append(cat.Arches, view)
	}
	return cat
}

func paths(entries []manifestEntry) []string {
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.PoolPath)
	}
	return result
}

func TestRepoCatalogEntryLookup(t *testing.T) {
	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {{name: "nano", path: "pool/main/n/nano/nano_7.2_amd64.deb"}},
	})

	tests := []struct {
		path string
		want bool
	}{
		{"pool/main/n/nano/nano_7.2_amd64.deb", true},
		{"pool/main/v/vim/vim_9_amd64.deb", false},
		{"", false},
		{"dists/tinyrepo/Release", false},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			if _, ok := cat.entry(tc.path); ok != tc.want {
				t.Errorf("entry(%q) found = %v, want %v", tc.path, ok, tc.want)
			}
		})
	}
}

// declared is what -dp downloads and what -cl keeps, so it has to be the full
// transitive closure and nothing else.
func TestRepoCatalogDeclaredIsTheClosure(t *testing.T) {
	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {
			{name: "a", path: "pool/a.deb", deps: []string{"b"}},
			{name: "b", path: "pool/b.deb", deps: []string{"c"}},
			{name: "c", path: "pool/c.deb"},
			{name: "unrelated", path: "pool/unrelated.deb"},
		},
	})

	got := paths(cat.declared([]string{"a"}))
	want := []string{"pool/a.deb", "pool/b.deb", "pool/c.deb"}

	if !slices.Equal(got, want) {
		t.Errorf("declared([a]) = %v, want %v", got, want)
	}
}

// A package published for two architectures under the same path - an
// "Architecture: all" .deb - must be listed once, not once per architecture.
func TestRepoCatalogDeduplicatesSharedPaths(t *testing.T) {
	shared := "pool/main/t/tzdata/tzdata_2024a_all.deb"

	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {{name: "tzdata", path: shared}},
		"i386":  {{name: "tzdata", path: shared}},
	})

	got := paths(cat.declared([]string{"tzdata"}))
	if len(got) != 1 || got[0] != shared {
		t.Errorf("declared = %v, want exactly [%s]", got, shared)
	}
}

// The same name maps to a different file in each architecture, which is exactly
// why the dependency views cannot be flattened into one map.
func TestRepoCatalogKeepsArchitecturesApart(t *testing.T) {
	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {
			{name: "nano", path: "pool/nano_7.2_amd64.deb", deps: []string{"libc"}},
			{name: "libc", path: "pool/libc_2.36_amd64.deb"},
		},
		"i386": {
			{name: "nano", path: "pool/nano_7.2_i386.deb", deps: []string{"libc"}},
			{name: "libc", path: "pool/libc_2.36_i386.deb"},
		},
	})

	got := paths(cat.declared([]string{"nano"}))
	want := []string{
		"pool/libc_2.36_amd64.deb",
		"pool/libc_2.36_i386.deb",
		"pool/nano_7.2_amd64.deb",
		"pool/nano_7.2_i386.deb",
	}

	if !slices.Equal(got, want) {
		t.Errorf("declared([nano]) = %v, want %v", got, want)
	}

	// An amd64 miss must pull the amd64 dependency, never the i386 one.
	closure := paths(cat.closure("pool/nano_7.2_amd64.deb"))
	if !slices.Equal(closure, []string{"pool/libc_2.36_amd64.deb"}) {
		t.Errorf("closure(amd64 nano) = %v, want only the amd64 libc", closure)
	}
}

// The closure is what the background thread warms, so it must not include the
// package that was just downloaded.
func TestRepoCatalogClosureExcludesItself(t *testing.T) {
	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {
			{name: "a", path: "pool/a.deb", deps: []string{"b"}},
			{name: "b", path: "pool/b.deb"},
		},
	})

	got := paths(cat.closure("pool/a.deb"))
	if !slices.Equal(got, []string{"pool/b.deb"}) {
		t.Errorf("closure(pool/a.deb) = %v, want [pool/b.deb]", got)
	}

	if got := cat.closure("pool/unknown.deb"); len(got) != 0 {
		t.Errorf("closure of an unknown path = %v, want nothing", got)
	}
}

// The generated index has to be byte-identical across runs, which starts here.
func TestRepoCatalogDeclaredIsStable(t *testing.T) {
	cat := buildRepoCatalog(map[string][]catalogPkg{
		"amd64": {
			{name: "a", path: "pool/a.deb", deps: []string{"b", "c"}},
			{name: "b", path: "pool/b.deb"},
			{name: "c", path: "pool/c.deb"},
		},
	})

	first := paths(cat.declared([]string{"a"}))
	for range 5 {
		if got := paths(cat.declared([]string{"a"})); !slices.Equal(got, first) {
			t.Fatalf("declared is not stable: %v then %v", first, got)
		}
	}
}

// The on-demand server resolves from several requests at once. resolveNames
// used to sort Provides in place, which made that a data race.
func TestCatalogResolveIsConcurrencySafe(t *testing.T) {
	cat := newCatalog()
	for i := range 50 {
		name := "pkg" + strconv.Itoa(i)
		cat.addPackage(name, [][]string{{"virtual"}}, []string{"virtual"}, nil)
	}

	var wg sync.WaitGroup
	results := make([][]string, 16)

	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = cat.resolveNames([]string{"pkg0"})
		}()
	}
	wg.Wait()

	// Every goroutine must also agree, otherwise the provider choice depended
	// on who got there first.
	for i, got := range results {
		if !slices.Equal(got, results[0]) {
			t.Fatalf("goroutine %d resolved %v, goroutine 0 resolved %v", i, got, results[0])
		}
	}
}
