package tinyrepo

import (
	"maps"
	"slices"
	"sync"
)

// catalog is the format-neutral view of an index that dependency resolution
// needs. Both the Debian and the pacman backends normalise into it at parse
// time, so the resolver itself never has to know which format it came from.
type catalog struct {
	// Deps maps a package name to its dependency clauses. Each clause is a set
	// of alternatives, any one of which satisfies it, already stripped of
	// version constraints. A package with no dependencies still has an entry,
	// so the keys of Deps are exactly the packages that exist.
	Deps map[string][][]string

	// Provides maps a virtual name to the packages that provide it. In pacman
	// this also carries soname dependencies such as "libreadline.so", which
	// cannot be resolved any other way.
	Provides map[string][]string

	// Groups maps a group name to its members. Groups are only meaningful as
	// seeds: neither format lets a package depend on a group.
	Groups map[string][]string

	// sortOnce guards the one mutation resolveNames makes, so the on-demand
	// server can resolve from several requests at the same time.
	sortOnce sync.Once
}

func newCatalog() *catalog {
	return &catalog{
		Deps:     map[string][][]string{},
		Provides: map[string][]string{},
		Groups:   map[string][]string{},
	}
}

// addPackage registers a package. The first registration of a name wins, which
// matches the order the indexes are read in.
func (c *catalog) addPackage(name string, deps [][]string, provides []string, groups []string) {
	if name == "" {
		return
	}
	if _, exists := c.Deps[name]; exists {
		return
	}
	c.Deps[name] = deps

	for _, virtual := range provides {
		if virtual != "" && virtual != name {
			c.Provides[virtual] = append(c.Provides[virtual], name)
		}
	}
	for _, group := range groups {
		if group != "" {
			c.Groups[group] = append(c.Groups[group], name)
		}
	}
}

// sortProviders orders every provider list. Providers accumulate in index
// order, so sorting them keeps the choice of provider - and therefore the whole
// resolution - stable across runs.
func (c *catalog) sortProviders() {
	c.sortOnce.Do(func() {
		for virtual := range c.Provides {
			slices.Sort(c.Provides[virtual])
		}
	})
}

// satisfy returns the real package that should be pulled in to satisfy a
// clause, preferring a package with that exact name over a provider.
func (c *catalog) satisfy(alternatives []string) (string, bool) {
	for _, name := range alternatives {
		if _, ok := c.Deps[name]; ok {
			return name, true
		}
	}
	for _, name := range alternatives {
		if providers := c.Provides[name]; len(providers) > 0 {
			return providers[0], true
		}
	}
	return "", false
}

// resolveNames walks the dependency graph breadth-first from the seeds, which
// may name either a package or a group. It returns the selected packages sorted
// by name, plus the names nothing could satisfy.
//
// Everything that reaches the queue is a package that exists, because want() is
// the only way in and it resolves virtual names first.
//
// It is safe to call from several goroutines at once, which the on-demand
// server does: sortProviders is the only write, and it happens once.
func (c *catalog) resolveNames(seeds []string) (selected []string, unresolved []string) {
	c.sortProviders()

	chosen := map[string]struct{}{}
	missing := map[string]struct{}{}
	var queue []string

	want := func(alternatives []string) {
		if len(alternatives) == 0 {
			return
		}
		if name, ok := c.satisfy(alternatives); ok {
			queue = append(queue, name)
			return
		}
		missing[alternatives[0]] = struct{}{}
	}

	for _, seed := range seeds {
		if members, ok := c.Groups[seed]; ok {
			for _, member := range members {
				want([]string{member})
			}
			continue
		}
		want([]string{seed})
	}

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]

		if _, done := chosen[name]; done {
			continue
		}
		chosen[name] = struct{}{}

		for _, alternatives := range c.Deps[name] {
			want(alternatives)
		}
	}

	return slices.Sorted(maps.Keys(chosen)), slices.Sorted(maps.Keys(missing))
}
