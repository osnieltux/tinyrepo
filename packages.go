package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const maxIndexLineSize = 1024 * 1024

// errNoPackagesSelected means the requested packages matched nothing in the
// cached indexes.
var errNoPackagesSelected = errors.New("no packages selected")

// dependencyReader parses a Depends/Pre-Depends field into clauses of
// alternatives, dropping version constraints and architecture qualifiers.
//
//	"libc6 (>= 2.34), perl:any | perl-base"  ->  [["libc6"], ["perl", "perl-base"]]
func dependencyReader(dependency string) [][]string {
	var clauses [][]string

	for _, clause := range strings.Split(dependency, ",") {
		var alternatives []string

		for _, alternative := range strings.Split(clause, "|") {
			name := strings.TrimSpace(alternative)

			// Cut at the first version constraint "(", architecture list "[",
			// build profile "<" or plain separator.
			if i := strings.IndexAny(name, " \t([<"); i != -1 {
				name = name[:i]
			}
			// "perl:any" -> "perl"
			if i := strings.IndexByte(name, ':'); i != -1 {
				name = name[:i]
			}

			if name = strings.TrimSpace(name); name != "" {
				alternatives = append(alternatives, name)
			}
		}

		if len(alternatives) > 0 {
			clauses = append(clauses, alternatives)
		}
	}
	return clauses
}

// indexArchFromPath derives the architecture from a ".../binary-<arch>/Packages"
// path.
func indexArchFromPath(filePath string) (string, error) {
	dir := filepath.Base(filepath.Dir(filePath))

	arch, ok := strings.CutPrefix(dir, "binary-")
	if !ok || arch == "" {
		return "", fmt.Errorf("%s %s", _t("architecture n f"), filePath)
	}
	return arch, nil
}

// parsePackages reads a Debian "Packages" index. indexArch is the architecture
// of the index itself; baseURL is the mirror it was fetched from.
func parsePackages(r io.Reader, indexArch, baseURL string) ([]*PackageDeb, error) {
	var packages []*PackageDeb
	var raw strings.Builder
	current := &PackageDeb{IndexArch: indexArch, FilenameUrl: baseURL}
	started := false

	flush := func() {
		defer raw.Reset()

		// A stanza with no Package: name is not a package.
		if !started || current.Package == "" {
			current = &PackageDeb{IndexArch: indexArch, FilenameUrl: baseURL}
			started = false
			return
		}
		if current.Arch == "" {
			current.Arch = indexArch
		}
		if KeepRawStanzas {
			// Plus the blank line that terminates a stanza, so the captured
			// text can be concatenated straight into an index.
			current.Raw = raw.String() + "\n"
		}
		packages = append(packages, current)
		current = &PackageDeb{IndexArch: indexArch, FilenameUrl: baseURL}
		started = false
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, maxIndexLineSize), maxIndexLineSize)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if KeepRawStanzas {
			raw.WriteString(line)
			raw.WriteByte('\n')
		}
		if parseField(current, line) {
			started = true
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Flush the final stanza: an index does not have to end with a blank line,
	// and without this the last package is silently dropped.
	flush()

	return packages, nil
}

// parseField fills one field of a stanza. It reports whether the line was a
// recognised field, so continuation lines do not start a stanza on their own.
func parseField(p *PackageDeb, line string) bool {
	if value, ok := strings.CutPrefix(line, "Package: "); ok {
		p.Package = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Version: "); ok {
		p.Version = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Architecture: "); ok {
		p.Arch = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Installed-Size: "); ok {
		p.InstalledSize = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Maintainer: "); ok {
		p.Maintainer = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Filename: "); ok {
		p.Filename = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Size: "); ok {
		p.Size = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "MD5sum: "); ok {
		p.MD5sum = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "SHA256: "); ok {
		p.SHA256 = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Pre-Depends: "); ok {
		p.Pre_Depends = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Depends: "); ok {
		p.Depends = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Provides: "); ok {
		p.Provides = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Breaks: "); ok {
		p.Breaks = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Homepage: "); ok {
		p.Homepage = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Description: "); ok {
		p.Description = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Tag: "); ok {
		p.Tag = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Section: "); ok {
		p.Section = value
		return true
	}
	if value, ok := strings.CutPrefix(line, "Priority: "); ok {
		p.Priority = value
		return true
	}
	return false
}

// readDebPackages reads one cached index file from disk.
func readDebPackages(filePath string) ([]*PackageDeb, error) {
	indexArch, err := indexArchFromPath(filePath)
	if err != nil {
		return nil, err
	}

	baseURL, err := readURLBase(filePath)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return parsePackages(file, indexArch, baseURL)
}

// readURLBase reads the mirror URL recorded for the dist owning this index.
// Layout: <dest>/dists_cache/<dist>/<component>/binary-<arch>/Packages
func readURLBase(filePath string) (string, error) {
	distDir := filepath.Dir(filepath.Dir(filepath.Dir(filePath)))
	path := filepath.Join(distDir, DistsCacheNameUrlBase)

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	baseURL, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimRight(strings.TrimSpace(baseURL), "/"), nil
}

// debCatalog builds the format-neutral view the shared resolver works on.
func debCatalog(index map[string]*PackageDeb) *catalog {
	cat := newCatalog()

	// Sorted so that, when two packages provide the same virtual name, the
	// choice does not depend on Go's map iteration order.
	for _, name := range slices.Sorted(maps.Keys(index)) {
		pkg := index[name]

		// Pre-Depends is as mandatory as Depends: skipping it leaves the
		// repository missing essential packages.
		var deps [][]string
		for _, field := range []string{pkg.Pre_Depends, pkg.Depends} {
			deps = append(deps, dependencyReader(field)...)
		}

		cat.addPackage(pkg.Package, deps, flattenClauses(dependencyReader(pkg.Provides)), nil)
	}
	return cat
}

// flattenClauses reduces parsed clauses to a plain list of names. Fields like
// Provides list names rather than alternatives, so one name per clause is right.
func flattenClauses(clauses [][]string) []string {
	names := make([]string, 0, len(clauses))
	for _, alternatives := range clauses {
		names = append(names, alternatives...)
	}
	return names
}

// resolveDependencies walks the dependency graph breadth-first from the seed
// package names. It returns the selected packages sorted by name, plus the
// names nothing could satisfy.
func resolveDependencies(index map[string]*PackageDeb, seeds []string) ([]*PackageDeb, []string) {
	names, unresolved := debCatalog(index).resolveNames(seeds)

	// Sorted output keeps the generated Packages byte-identical across runs.
	result := make([]*PackageDeb, 0, len(names))
	for _, name := range names {
		if pkg, ok := index[name]; ok {
			result = append(result, pkg)
		}
	}
	return result, unresolved
}

// loadDebIndex reads every cached Packages file and returns, per index
// architecture, the "name -> package" map the later stages work from. The first
// occurrence of a name wins, matching the order the indexes are read in.
func loadDebIndex(config *Config) (map[string]map[string]*PackageDeb, error) {
	cachePath := filepath.Join(config.Destination.Path, DistsCacheName)

	indexFiles, err := recursiveFinder(cachePath, "Packages")
	if err != nil {
		return nil, fmt.Errorf("%s %v", _t("err recursive f"), err)
	}
	if len(indexFiles) == 0 {
		return nil, fmt.Errorf("%s %s", _t("no p f"), cachePath)
	}

	byArch := map[string]map[string]*PackageDeb{}

	for _, indexFile := range indexFiles {
		packages, err := readDebPackages(indexFile)
		if err != nil {
			logErrorf("%s %s: %v", _t("error r p"), indexFile, err)
			continue
		}

		for _, pkg := range packages {
			index, ok := byArch[pkg.IndexArch]
			if !ok {
				index = map[string]*PackageDeb{}
				byArch[pkg.IndexArch] = index
			}
			if _, exists := index[pkg.Package]; !exists {
				index[pkg.Package] = pkg
			}
		}
	}
	return byArch, nil
}

// createDists reads the cached indexes and returns what to publish in the index
// and what to record in the manifest, independently for each configured
// architecture.
//
// The two are the same set unless on-demand mode is on, in which case the index
// advertises every package the mirror has while the manifest still holds only
// the declared closure. That gap is the whole point of on demand: a client can
// only ask for a package it can see, so it has to see the ones we do not have.
func createDists(config *Config) (published, declared map[string][]*PackageDeb, err error) {
	byArch, err := loadDebIndex(config)
	if err != nil {
		return nil, nil, err
	}

	published = map[string][]*PackageDeb{}
	declared = map[string][]*PackageDeb{}
	total := 0

	for _, arch := range config.Destination.Arch {
		index := byArch[arch]
		if len(index) == 0 {
			logError(_t("arch no p"), arch)
			continue
		}

		packages, unresolved := resolveDependencies(index, config.Destination.Packages)
		if len(unresolved) > 0 {
			// Silently dropping these is how a repo ends up subtly incomplete.
			logError(_t("unresolved"), arch, strings.Join(unresolved, " "))
		}
		declared[arch] = packages

		if config.Settings.OnDemand {
			published[arch] = allDebPackages(index)
			logDebugf("%s %s: %d", _t("publishing all"), arch, len(published[arch]))
		} else {
			published[arch] = packages
		}

		total += len(published[arch])
		logDebugf("%s %s: %d", _t("selected"), arch, len(declared[arch]))
	}

	if total == 0 {
		return nil, nil, errNoPackagesSelected
	}
	return published, declared, nil
}

// allDebPackages returns every package of an index, sorted by name so the
// generated Packages file is byte-identical across runs.
func allDebPackages(index map[string]*PackageDeb) []*PackageDeb {
	packages := make([]*PackageDeb, 0, len(index))
	for _, name := range slices.Sorted(maps.Keys(index)) {
		packages = append(packages, index[name])
	}
	return packages
}
