package tinyrepo

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// repoCatalog is the full upstream catalog read from the local cache: every
// package the mirror publishes, keyed by the path a client will ask for.
//
// It deliberately holds neither *PackageDeb nor *archEntry. A whole Debian
// suite is hundreds of megabytes once parsed, and the on-demand server keeps
// this alive for as long as it runs, so it stores only what it actually needs:
// where to fetch a file and how to verify it.
type repoCatalog struct {
	// Files is every package the mirror publishes, keyed by the request path
	// with no leading slash. Debian looks like
	// "pool/main/n/nano/nano_7.2-1_amd64.deb"; pacman is a bare file name,
	// because those repositories are flat.
	Files map[string]manifestEntry

	// Arches holds one dependency view per configured architecture. This cannot
	// be flattened: the same package name maps to a different path in each
	// architecture, and both formats resolve one architecture at a time.
	Arches []*archCatalogView
}

// archCatalogView is the dependency graph of a single architecture, plus the
// mapping between package names and the paths they are published at.
type archCatalogView struct {
	Arch  string
	Deps  *catalog
	Paths map[string]string // package name -> request path
	Names map[string]string // request path -> package name
}

func newRepoCatalog() *repoCatalog {
	return &repoCatalog{Files: map[string]manifestEntry{}}
}

func newArchCatalogView(arch string, deps *catalog) *archCatalogView {
	return &archCatalogView{
		Arch:  arch,
		Deps:  deps,
		Paths: map[string]string{},
		Names: map[string]string{},
	}
}

// add records one package: its dependency view entry and, the first time the
// path is seen, how to fetch it. An "Architecture: all" .deb appears in every
// architecture's index under the same path, so first occurrence wins.
func (c *repoCatalog) add(view *archCatalogView, name string, entry manifestEntry) {
	if name == "" || entry.PoolPath == "" {
		return
	}
	// The path comes from the mirror's index, and the prefetcher writes to it
	// without a client ever being involved, so it is checked on the way in.
	if !safeRelPath(entry.PoolPath) {
		logError(_t("err unsafe path"), entry.PoolPath)
		return
	}
	view.Paths[name] = entry.PoolPath
	view.Names[entry.PoolPath] = name

	if _, exists := c.Files[entry.PoolPath]; !exists {
		c.Files[entry.PoolPath] = entry
	}
}

// entry returns how to fetch the package published at a request path.
func (c *repoCatalog) entry(requestPath string) (manifestEntry, bool) {
	if c == nil {
		return manifestEntry{}, false
	}
	e, ok := c.Files[requestPath]
	return e, ok
}

// declared returns the seeds and their dependency closure, across every
// architecture, deduplicated by path and sorted. It is exactly what -dp
// downloads and exactly what -cl keeps.
func (c *repoCatalog) declared(seeds []string) []manifestEntry {
	paths := map[string]struct{}{}

	for _, view := range c.Arches {
		names, unresolved := view.Deps.resolveNames(seeds)
		if len(unresolved) > 0 {
			logDebugf("%s %s: %d", _t("unresolved"), view.Arch, len(unresolved))
		}
		for _, name := range names {
			if path, ok := view.Paths[name]; ok {
				paths[path] = struct{}{}
			}
		}
	}
	return c.entriesFor(paths)
}

// closure returns the packages that should be warm alongside the one published
// at requestPath: its dependency closure within its own architecture, minus
// itself. An unknown path returns nothing.
func (c *repoCatalog) closure(requestPath string) []manifestEntry {
	paths := map[string]struct{}{}

	for _, view := range c.Arches {
		name, ok := view.Names[requestPath]
		if !ok {
			continue
		}
		names, _ := view.Deps.resolveNames([]string{name})
		for _, dependency := range names {
			if path, ok := view.Paths[dependency]; ok && path != requestPath {
				paths[path] = struct{}{}
			}
		}
	}
	return c.entriesFor(paths)
}

// entriesFor turns a set of paths into manifest entries, sorted so the result
// is the same on every run.
func (c *repoCatalog) entriesFor(paths map[string]struct{}) []manifestEntry {
	entries := make([]manifestEntry, 0, len(paths))
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		if entry, ok := c.Files[path]; ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// Catalog reads every cached Packages file and indexes it by pool path.
func (debianBackend) Catalog(config *Config) (*repoCatalog, error) {
	byArch, err := loadDebIndex(config)
	if err != nil {
		return nil, err
	}

	cat := newRepoCatalog()

	for _, arch := range config.Destination.Arch {
		index := byArch[arch]
		if len(index) == 0 {
			logError(_t("arch no p"), arch)
			continue
		}

		view := newArchCatalogView(arch, debCatalog(index))
		for _, name := range slices.Sorted(maps.Keys(index)) {
			pkg := index[name]
			// Size is a string upstream; a malformed one just means the size
			// check is skipped, the SHA256 still protects the download.
			size, _ := strconv.ParseInt(pkg.Size, 10, 64)
			cat.add(view, name, manifestEntry{
				BaseURL:  pkg.FilenameUrl,
				PoolPath: pkg.Filename,
				Size:     size,
				SHA256:   pkg.SHA256,
			})
		}
		cat.Arches = append(cat.Arches, view)
	}

	if len(cat.Arches) == 0 {
		return nil, fmt.Errorf("%s %s", _t("no p f"), config.Destination.Path)
	}
	return cat, nil
}

// Catalog reads every cached pacman database and indexes it by file name.
func (archBackend) Catalog(config *Config) (*repoCatalog, error) {
	cat := newRepoCatalog()

	for _, arch := range config.Destination.Arch {
		// The .files databases are ten times bigger and carry nothing the
		// catalog needs, so they are never read here.
		index, err := loadArchIndexOpt(config, arch, false)
		if err != nil {
			return nil, err
		}

		view := newArchCatalogView(arch, index.catalog())
		for _, name := range slices.Sorted(maps.Keys(index.ByName)) {
			entry := index.ByName[name]
			cat.add(view, name, manifestEntry{
				BaseURL:  entry.BaseURL,
				PoolPath: entry.Filename,
				Size:     entry.Size,
				SHA256:   entry.SHA256,
			})
		}
		cat.Arches = append(cat.Arches, view)
	}

	if len(cat.Arches) == 0 {
		return nil, fmt.Errorf("%s %s", _t("no p f"), config.Destination.Path)
	}
	return cat, nil
}
