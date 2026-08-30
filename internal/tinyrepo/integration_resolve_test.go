package tinyrepo

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Dependency resolution invariants on real data.
// See integration_test.go for the shared fixture and scaffolding.

// integrationDepIndex loads the cached fixture index into the same
// "name -> package" map that createDists builds internally, so a test can call
// resolveDependencies directly and see the unresolved names that createDists
// only logs.
func integrationDepIndex(t *testing.T, config *Config) map[string]*PackageDeb {
	t.Helper()

	path := cachedIndexPath(config)
	packages, err := readDebPackages(path)
	if err != nil {
		t.Fatalf("readDebPackages(%s): %v", path, err)
	}

	index := make(map[string]*PackageDeb, len(packages))
	for _, pkg := range packages {
		// First occurrence wins, exactly as createDists does.
		if _, exists := index[pkg.Package]; !exists {
			index[pkg.Package] = pkg
		}
	}

	// A truncated or empty cache would make every invariant below pass
	// vacuously, so refuse to run instead.
	if len(index) < 100 {
		t.Fatalf("%s: parsed %d distinct packages, want more than 100: the cached %s/%s slice looks truncated",
			path, len(index), fixtureDist, fixtureComponent)
	}
	return index
}

// integrationDepNames is the package names of a resolution result, in order.
func integrationDepNames(packages []*PackageDeb) []string {
	names := make([]string, 0, len(packages))
	for _, pkg := range packages {
		names = append(names, pkg.Package)
	}
	return names
}

// integrationDepFields is the dependency fields the resolver must honour, in
// the order it walks them.
func integrationDepFields(pkg *PackageDeb) []struct{ Name, Value string } {
	return []struct{ Name, Value string }{
		{"Pre-Depends", pkg.Pre_Depends},
		{"Depends", pkg.Depends},
	}
}

// integrationDepReferenced is every dependency name mentioned by the given
// packages, across both Depends and Pre-Depends and including every
// alternative of every clause. It is deliberately a superset of what the
// resolver actually queues.
func integrationDepReferenced(packages []*PackageDeb) map[string]bool {
	referenced := map[string]bool{}

	for _, pkg := range packages {
		for _, field := range integrationDepFields(pkg) {
			for _, alternatives := range dependencyReader(field.Value) {
				for _, name := range alternatives {
					referenced[name] = true
				}
			}
		}
	}
	return referenced
}

// integrationDepAlternativeSeeds finds the packages that can only be satisfied
// by a non-first alternative: their clause names something absent from this
// component first and something present later. Seeding with exactly these, and
// not with the alternatives themselves, is the only way to observe that the
// resolver really looks the alternatives up instead of blindly taking the first
// name in the clause.
func integrationDepAlternativeSeeds(index map[string]*PackageDeb) []string {
	var seeds []string

	for _, name := range slices.Sorted(maps.Keys(index)) {
		pkg := index[name]

		for _, field := range integrationDepFields(pkg) {
			for _, alternatives := range dependencyReader(field.Value) {
				if len(alternatives) < 2 {
					continue
				}
				if _, first := index[alternatives[0]]; first {
					continue
				}
				for _, later := range alternatives[1:] {
					if _, ok := index[later]; ok {
						seeds = append(seeds, name)
						break
					}
				}
			}
		}
	}
	return slices.Compact(seeds)
}

// TestIntegrationEveryDependencyClauseIsAccountedFor is the central invariant:
// on real data, no dependency may be silently dropped. For every selected
// package, every clause of Depends and Pre-Depends must end up either
// satisfied - some alternative that exists in this component was selected - or
// reported, when the whole clause lives outside the component.
//
// Which alternative was picked is left to the resolver, so the test does not
// re-derive apt's preference order; but a clause that could have been satisfied
// and instead ended up in the unresolved list is a real defect.
//
// It runs over three seed sets, because no single one reaches every clause
// shape: the configured seed, which is the realistic partial selection; the
// packages that can only be satisfied by a non-first alternative; and the whole
// index, which reaches the Pre-Depends field that the seed's own closure never
// exercises.
func TestIntegrationEveryDependencyClauseIsAccountedFor(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	index := integrationDepIndex(t, config)

	// A partial selection that reaches clauses whose first alternative is
	// missing: without it, the whole-index scenario seeds those alternatives
	// directly and can no longer tell whether the resolver consulted them.
	alternativeSeeds := integrationDepAlternativeSeeds(index)
	if len(alternativeSeeds) == 0 {
		t.Logf("no package in %s/%s needs a non-first alternative; that scenario is skipped",
			fixtureDist, fixtureComponent)
	}

	scenarios := []struct {
		name  string
		seeds []string
	}{
		{"from the seed", config.Destination.Packages},
		{"from packages that need a non-first alternative", alternativeSeeds},
		{"from every package in the index", slices.Sorted(maps.Keys(index))},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if len(scenario.seeds) == 0 {
				t.Skip("no seeds for this scenario in the current index")
			}

			selected, unresolved := resolveDependencies(index, scenario.seeds)
			if len(selected) == 0 {
				t.Fatalf("resolveDependencies(%d seeds): selected nothing from an index of %d packages",
					len(scenario.seeds), len(index))
			}

			isSelected := make(map[string]bool, len(selected))
			for _, pkg := range selected {
				isSelected[pkg.Package] = true
			}
			isReported := make(map[string]bool, len(unresolved))
			for _, name := range unresolved {
				isReported[name] = true
			}

			// A name can be satisfied by a package called something else:
			// nvidia-open-kernel-support--v1 exists only as a Provides of
			// nvidia-open-kernel-support.
			providers := map[string][]string{}
			for name, pkg := range index {
				for _, virtual := range flattenClauses(dependencyReader(pkg.Provides)) {
					providers[virtual] = append(providers[virtual], name)
				}
			}

			// candidates lists the real packages in the index that can satisfy
			// a dependency name, directly or through Provides.
			candidates := func(name string) []string {
				if _, ok := index[name]; ok {
					return []string{name}
				}
				return providers[name]
			}

			clauses, satisfiable, preDependsClauses, alternativeClauses := 0, 0, 0, 0

			for _, pkg := range selected {
				for _, field := range integrationDepFields(pkg) {
					for _, alternatives := range dependencyReader(field.Value) {
						clauses++
						if field.Name == "Pre-Depends" {
							preDependsClauses++
						}
						if len(alternatives) > 1 {
							alternativeClauses++
						}

						// The packages this component could actually use to
						// satisfy the clause.
						var present []string
						for _, name := range alternatives {
							present = append(present, candidates(name)...)
						}

						if len(present) == 0 {
							// Nothing here is available: the clause must show
							// up in the unresolved report, not vanish.
							reported := false
							for _, name := range alternatives {
								if isReported[name] {
									reported = true
									break
								}
							}
							if !reported {
								t.Errorf("%s: %s clause %v was dropped silently: no alternative exists in %s and none was reported as unresolved (whole field: %q)",
									pkg.Package, field.Name, alternatives, fixtureComponent, field.Value)
							}
							continue
						}

						satisfiable++

						satisfied := false
						for _, name := range present {
							if isSelected[name] {
								satisfied = true
								break
							}
						}
						if !satisfied {
							t.Errorf("%s: %s clause %v could have been satisfied by %v from %s, but none of them was selected (whole field: %q)",
								pkg.Package, field.Name, alternatives, present, fixtureComponent, field.Value)
						}
					}
				}
			}

			// Guard against the invariant passing vacuously if upstream ever
			// stops publishing dependency fields for this component, or moves
			// every dependency out of it.
			if clauses == 0 {
				t.Fatalf("checked 0 dependency clauses over %d selected packages", len(selected))
			}
			if satisfiable == 0 {
				t.Fatalf("none of the %d dependency clauses over %d selected packages resolves inside %s, so only the reporting half was exercised",
					clauses, len(selected), fixtureComponent)
			}
			t.Logf("checked %d clauses (%d satisfiable inside %s, %d Pre-Depends, %d with alternatives) over %d selected packages, %d unresolved",
				clauses, satisfiable, fixtureComponent, preDependsClauses, alternativeClauses, len(selected), len(unresolved))
		})
	}
}

// TestIntegrationSeedFansOutToItsRealSiblings pins the fixture itself: the seed
// must exist, must fan out transitively rather than resolve to itself alone,
// and every package it drags in must carry the fields the rest of the pipeline
// needs. Clause-by-clause correctness is covered by
// TestIntegrationEveryDependencyClauseIsAccountedFor; this test exists so a
// broken fixture fails here, loudly and by name, instead of quietly weakening
// every other test.
func TestIntegrationSeedFansOutToItsRealSiblings(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	index := integrationDepIndex(t, config)

	seed, ok := index[fixtureSeed]
	if !ok {
		t.Fatalf("seed %q is not in the cached %s/%s index", fixtureSeed, fixtureDist, fixtureComponent)
	}

	selected, _ := resolveDependencies(index, []string{fixtureSeed})
	names := integrationDepNames(selected)

	if !slices.Contains(names, fixtureSeed) {
		t.Fatalf("the seed %q is missing from its own closure %v", fixtureSeed, names)
	}
	if len(selected) < 2 {
		t.Fatalf("%s resolved to %v alone: the fixture is supposed to fan out inside %s (Pre-Depends: %q, Depends: %q)",
			fixtureSeed, names, fixtureComponent, seed.Pre_Depends, seed.Depends)
	}

	// Everything selected must carry the fields the pool download and the
	// generated index need. This is real archive data, so these are never empty.
	for _, pkg := range selected {
		if pkg.Version == "" {
			t.Errorf("%s: empty Version in the real index", pkg.Package)
		}
		if pkg.Filename == "" {
			t.Errorf("%s: empty Filename, the .deb could never be fetched", pkg.Package)
		}
		if pkg.SHA256 == "" {
			t.Errorf("%s: empty SHA256, the download could never be verified", pkg.Package)
		}
		if pkg.FilenameUrl != fixtureMirror {
			t.Errorf("%s: FilenameUrl = %q, want %q (recorded in %s)",
				pkg.Package, pkg.FilenameUrl, fixtureMirror, DistsCacheNameUrlBase)
		}
		if pkg.IndexArch != fixtureArch {
			t.Errorf("%s: IndexArch = %q, want %q (it was read from the binary-%s index)",
				pkg.Package, pkg.IndexArch, fixtureArch, fixtureArch)
		}
	}
}

// TestIntegrationUnresolvedNamesAreRealAndReported covers the other half of the
// fixture: the packages in this component depend on packages that live in main,
// which is not part of the slice, so the closure must report them instead of
// pretending the repository is complete.
func TestIntegrationUnresolvedNamesAreRealAndReported(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	index := integrationDepIndex(t, config)

	selected, unresolved := resolveDependencies(index, config.Destination.Packages)

	if len(unresolved) == 0 {
		t.Fatalf("resolveDependencies(seeds=%v) reported no unresolved names, but the %s slice cannot satisfy dependencies that live in other components; closure was %v",
			config.Destination.Packages, fixtureComponent, integrationDepNames(selected))
	}

	isSelected := make(map[string]bool, len(selected))
	for _, pkg := range selected {
		isSelected[pkg.Package] = true
	}

	referenced := integrationDepReferenced(selected)
	for _, name := range config.Destination.Packages {
		referenced[name] = true
	}

	for _, name := range unresolved {
		if _, present := index[name]; present {
			t.Errorf("%q was reported as unresolved but it is present in the %s index", name, fixtureComponent)
		}
		if isSelected[name] {
			t.Errorf("%q was reported as unresolved and selected at the same time", name)
		}
		if !referenced[name] {
			t.Errorf("%q was reported as unresolved but no seed and no selected package depends on it", name)
		}
	}

	for i := 1; i < len(unresolved); i++ {
		switch {
		case unresolved[i-1] == unresolved[i]:
			t.Errorf("unresolved name %q is reported twice, at %d and %d: %v", unresolved[i], i-1, i, unresolved)
		case unresolved[i-1] > unresolved[i]:
			t.Errorf("unresolved names are not sorted: %q at %d comes before %q at %d: %v",
				unresolved[i-1], i-1, unresolved[i], i, unresolved)
		}
	}
}

// TestIntegrationCreateDistsSelectionIsUniqueAndSorted drives the real
// createDists entry point over the cached slice.
func TestIntegrationCreateDistsSelectionIsUniqueAndSorted(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)

	selected, _, err := createDists(config)
	if err != nil {
		t.Fatalf("createDists: %v", err)
	}

	for arch := range selected {
		if !slices.Contains(config.Destination.Arch, arch) {
			t.Errorf("createDists returned architecture %q, which is not in %v", arch, config.Destination.Arch)
		}
	}

	packages := selected[fixtureArch]
	if len(packages) == 0 {
		t.Fatalf("createDists selected nothing for %s (keys: %v)", fixtureArch, slices.Sorted(maps.Keys(selected)))
	}

	// Sorted and duplicate-free in a single pass: a duplicate name would show up
	// as two adjacent equal entries.
	for i := 1; i < len(packages); i++ {
		switch {
		case packages[i-1].Package == packages[i].Package:
			t.Errorf("package %q is selected twice, at %d and %d", packages[i].Package, i-1, i)
		case packages[i-1].Package > packages[i].Package:
			t.Errorf("selection is not sorted by name: %q at %d comes before %q at %d",
				packages[i-1].Package, i-1, packages[i].Package, i)
		}
	}

	if !slices.Contains(integrationDepNames(packages), fixtureSeed) {
		t.Errorf("the requested package %q is not in the selection %v", fixtureSeed, integrationDepNames(packages))
	}
}

// TestIntegrationResolutionIsByteForByteStable runs the resolution repeatedly on
// the same real input. The generated Packages file must be reproducible, so the
// order of the result may not depend on map iteration order, which Go
// randomises per iteration.
func TestIntegrationResolutionIsByteForByteStable(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	index := integrationDepIndex(t, config)

	// Render through the production stanza writer, so "stable" means the bytes
	// that actually reach dists/, not just the names.
	render := func(packages []*PackageDeb) string {
		var b strings.Builder
		for _, pkg := range packages {
			writeStanza(&b, pkg)
		}
		return b.String()
	}

	scenarios := []struct {
		name  string
		seeds []string
	}{
		{"from the seed", config.Destination.Packages},
		{"from every package in the index", slices.Sorted(maps.Keys(index))},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			first, firstUnresolved := resolveDependencies(index, scenario.seeds)
			if len(first) == 0 {
				t.Fatalf("resolveDependencies(%d seeds): selected nothing", len(scenario.seeds))
			}
			firstBytes := render(first)

			for run := 1; run <= 5; run++ {
				next, nextUnresolved := resolveDependencies(index, scenario.seeds)

				if got := render(next); got != firstBytes {
					t.Fatalf("run %d produced different bytes than run 0: %d bytes vs %d, names %v vs %v",
						run, len(got), len(firstBytes), integrationDepNames(next), integrationDepNames(first))
				}
				if !slices.Equal(nextUnresolved, firstUnresolved) {
					t.Fatalf("run %d reported unresolved %v, run 0 reported %v", run, nextUnresolved, firstUnresolved)
				}
			}
		})
	}
}

// TestIntegrationSeedOrderDoesNotChangeTheClosure: a closure is a set, so
// repeating a seed or additionally naming a package that is already reachable
// must not change the result.
func TestIntegrationSeedOrderDoesNotChangeTheClosure(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	index := integrationDepIndex(t, config)

	base, baseUnresolved := resolveDependencies(index, []string{fixtureSeed})
	baseNames := integrationDepNames(base)

	if len(base) == 0 {
		t.Fatalf("seed %q resolved to nothing in the cached %s/%s index", fixtureSeed, fixtureDist, fixtureComponent)
	}

	// A package other than the seed that the seed already reaches, discovered
	// from the closure itself rather than hardcoded.
	reached := ""
	for _, name := range baseNames {
		if name != fixtureSeed {
			reached = name
			break
		}
	}
	if reached == "" {
		t.Fatalf("%s: closure is only the seed itself, so this test would prove nothing (Depends: %q)",
			fixtureSeed, index[fixtureSeed].Depends)
	}

	variants := [][]string{
		{fixtureSeed, fixtureSeed},
		{reached, fixtureSeed},
		{fixtureSeed, reached},
	}

	for _, seeds := range variants {
		got, unresolved := resolveDependencies(index, seeds)

		if names := integrationDepNames(got); !slices.Equal(names, baseNames) {
			t.Errorf("seeds %v produced %v, seeds [%s] produced %v", seeds, names, fixtureSeed, baseNames)
		}
		if !slices.Equal(unresolved, baseUnresolved) {
			t.Errorf("seeds %v reported unresolved %v, seeds [%s] reported %v",
				seeds, unresolved, fixtureSeed, baseUnresolved)
		}
	}
}

// TestIntegrationSeedingEveryPackageSelectsExactlyTheIndex: when every name in
// the index is a seed, the closure is forced to be exactly the index - no
// duplicates, nothing dropped, and nothing invented. It is the widest check
// that can be made without writing a single upstream value down.
func TestIntegrationSeedingEveryPackageSelectsExactlyTheIndex(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	index := integrationDepIndex(t, config)

	seeds := slices.Sorted(maps.Keys(index))
	selected, unresolved := resolveDependencies(index, seeds)

	seen := make(map[string]bool, len(selected))
	for _, pkg := range selected {
		if seen[pkg.Package] {
			t.Errorf("%q appears twice in the selection", pkg.Package)
		}
		seen[pkg.Package] = true

		want, ok := index[pkg.Package]
		if !ok {
			t.Errorf("%q was selected but it is not in the index it was resolved from", pkg.Package)
			continue
		}
		if pkg != want {
			t.Errorf("%q: the selection holds a different stanza than the index (version %q vs %q)",
				pkg.Package, pkg.Version, want.Version)
		}
	}

	for _, name := range seeds {
		if !seen[name] {
			t.Errorf("%q is in the index and was named as a seed, but it was not selected", name)
		}
	}
	if len(selected) != len(index) {
		t.Errorf("selected %d packages from an index of %d when seeding with every name",
			len(selected), len(index))
	}

	// Whatever is still unresolved must genuinely be outside the index.
	for _, name := range unresolved {
		if _, present := index[name]; present {
			t.Errorf("%q was reported as unresolved but it is present in the index", name)
		}
	}
}

// TestIntegrationUnknownSeedIsReported: asking for a package that does not exist
// must fail loudly instead of quietly producing an empty repository.
func TestIntegrationUnknownSeedIsReported(t *testing.T) {
	requireNetwork(t)

	const missing = "tinyrepo-no-such-package-4f2a"

	config := newRepo(t, missing)

	selected, _, err := createDists(config)
	if !errors.Is(err, errNoPackagesSelected) {
		t.Fatalf("createDists(packages=[%s]) returned %d selections and err=%v, want errNoPackagesSelected",
			missing, len(selected), err)
	}
}
