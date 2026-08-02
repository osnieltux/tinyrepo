package main

import (
	"slices"
	"testing"
)

// buildCatalog is a compact way to describe a catalog in a table test.
type pkgSpec struct {
	name     string
	deps     []string // one dependency per clause
	provides []string
	groups   []string
}

func buildCatalog(specs ...pkgSpec) *catalog {
	cat := newCatalog()

	for _, spec := range specs {
		clauses := make([][]string, 0, len(spec.deps))
		for _, dep := range spec.deps {
			clauses = append(clauses, []string{dep})
		}
		cat.addPackage(spec.name, clauses, spec.provides, spec.groups)
	}
	return cat
}

func TestResolveNamesFollowsDependencies(t *testing.T) {
	cat := buildCatalog(
		pkgSpec{name: "nano", deps: []string{"glibc", "ncurses"}},
		pkgSpec{name: "ncurses", deps: []string{"glibc"}},
		pkgSpec{name: "glibc"},
		pkgSpec{name: "unrelated"},
	)

	selected, unresolved := cat.resolveNames([]string{"nano"})

	if want := []string{"glibc", "nano", "ncurses"}; !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

func TestResolveNamesUsesProvides(t *testing.T) {
	// bash depends on the soname libreadline.so, which exists only as a
	// Provides of readline. Without provides support nothing resolves.
	cat := buildCatalog(
		pkgSpec{name: "bash", deps: []string{"libreadline.so", "sh"}},
		pkgSpec{name: "readline", provides: []string{"libreadline.so"}},
		pkgSpec{name: "dash", provides: []string{"sh"}},
	)

	selected, unresolved := cat.resolveNames([]string{"bash"})

	if want := []string{"bash", "dash", "readline"}; !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

func TestResolveNamesPrefersRealPackageOverProvider(t *testing.T) {
	// A package named awk must win over one that merely provides awk.
	cat := buildCatalog(
		pkgSpec{name: "seed", deps: []string{"awk"}},
		pkgSpec{name: "awk"},
		pkgSpec{name: "gawk", provides: []string{"awk"}},
	)

	selected, _ := cat.resolveNames([]string{"seed"})

	if slices.Contains(selected, "gawk") {
		t.Errorf("selected = %v, want the real awk rather than the provider", selected)
	}
	if !slices.Contains(selected, "awk") {
		t.Errorf("selected = %v, want it to contain awk", selected)
	}
}

func TestResolveNamesPicksProviderDeterministically(t *testing.T) {
	cat := buildCatalog(
		pkgSpec{name: "seed", deps: []string{"cron"}},
		pkgSpec{name: "zcron", provides: []string{"cron"}},
		pkgSpec{name: "acron", provides: []string{"cron"}},
		pkgSpec{name: "mcron", provides: []string{"cron"}},
	)

	first, _ := cat.resolveNames([]string{"seed"})
	for range 20 {
		next, _ := cat.resolveNames([]string{"seed"})
		if !slices.Equal(next, first) {
			t.Fatalf("provider choice changed between runs: %v != %v", next, first)
		}
	}
	// Sorted providers means the first alphabetically wins.
	if !slices.Contains(first, "acron") {
		t.Errorf("selected = %v, want the alphabetically first provider", first)
	}
}

func TestResolveNamesExpandsGroups(t *testing.T) {
	cat := buildCatalog(
		pkgSpec{name: "plasma-desktop", groups: []string{"plasma"}, deps: []string{"kwin"}},
		pkgSpec{name: "plasma-workspace", groups: []string{"plasma"}},
		pkgSpec{name: "kwin"},
		pkgSpec{name: "outside"},
	)

	selected, unresolved := cat.resolveNames([]string{"plasma"})

	want := []string{"kwin", "plasma-desktop", "plasma-workspace"}
	if !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

func TestResolveNamesAlternatives(t *testing.T) {
	cat := newCatalog()
	cat.addPackage("seed", [][]string{{"missing-one", "present-two"}}, nil, nil)
	cat.addPackage("present-two", nil, nil, nil)

	selected, unresolved := cat.resolveNames([]string{"seed"})

	if !slices.Contains(selected, "present-two") {
		t.Errorf("selected = %v, want the available alternative", selected)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v: a clause with one usable alternative is satisfied", unresolved)
	}
}

func TestResolveNamesReportsWhatItCannotSatisfy(t *testing.T) {
	cat := buildCatalog(
		pkgSpec{name: "seed", deps: []string{"absent"}},
	)

	selected, unresolved := cat.resolveNames([]string{"seed", "no-such-package"})

	if !slices.Equal(selected, []string{"seed"}) {
		t.Errorf("selected = %v", selected)
	}
	if want := []string{"absent", "no-such-package"}; !slices.Equal(unresolved, want) {
		t.Errorf("unresolved = %v, want %v", unresolved, want)
	}
}

func TestResolveNamesHandlesCycles(t *testing.T) {
	cat := buildCatalog(
		pkgSpec{name: "a", deps: []string{"b"}},
		pkgSpec{name: "b", deps: []string{"c"}},
		pkgSpec{name: "c", deps: []string{"a"}},
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		selected, _ := cat.resolveNames([]string{"a"})
		if want := []string{"a", "b", "c"}; !slices.Equal(selected, want) {
			t.Errorf("selected = %v, want %v", selected, want)
		}
	}()
	<-done
}

func TestResolveNamesDeduplicates(t *testing.T) {
	cat := buildCatalog(
		pkgSpec{name: "seed", deps: []string{"shared", "shared"}},
		pkgSpec{name: "shared"},
	)

	selected, _ := cat.resolveNames([]string{"seed", "seed", "shared"})

	if want := []string{"seed", "shared"}; !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
}

func TestCatalogFirstRegistrationWins(t *testing.T) {
	cat := newCatalog()
	cat.addPackage("dup", [][]string{{"first"}}, nil, nil)
	cat.addPackage("dup", [][]string{{"second"}}, nil, nil)

	if got := cat.Deps["dup"]; len(got) != 1 || got[0][0] != "first" {
		t.Errorf("Deps[dup] = %v, want the first registration to win", got)
	}
}

func TestCatalogIgnoresSelfProvides(t *testing.T) {
	// A package providing its own name would otherwise make the provides index
	// grow for nothing.
	cat := newCatalog()
	cat.addPackage("bash", nil, []string{"bash", "sh"}, nil)

	if _, ok := cat.Provides["bash"]; ok {
		t.Error("a package providing its own name was recorded in Provides")
	}
	if got := cat.Provides["sh"]; !slices.Equal(got, []string{"bash"}) {
		t.Errorf("Provides[sh] = %v", got)
	}
}
