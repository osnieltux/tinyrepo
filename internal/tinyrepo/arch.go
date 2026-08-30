package tinyrepo

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
)

// archTarget is one pacman repository database to mirror, expanded from a
// single [server].source line.
type archTarget struct {
	Template string // https://mirror/$repo/os/$arch
	Repo     string // core
	Arch     string // x86_64
}

// baseURL expands the mirror template the way pacman does.
//
// Using the same $repo/$arch placeholders as /etc/pacman.d/mirrorlist means a
// user can paste their existing Server line, and it covers both layouts:
// Arch is $mirror/$repo/os/$arch, Manjaro is $mirror/$branch/$repo/$arch.
func (t archTarget) baseURL() string {
	url := strings.ReplaceAll(t.Template, "$repo", t.Repo)
	url = strings.ReplaceAll(url, "$arch", t.Arch)
	return strings.TrimRight(url, "/")
}

func (t archTarget) dbURL() string    { return t.baseURL() + "/" + t.Repo + ".db" }
func (t archTarget) filesURL() string { return t.baseURL() + "/" + t.Repo + ".files" }

// cacheDir is where this repository's databases are cached locally.
func (t archTarget) cacheDir(destinationPath string) string {
	return filepath.Join(destinationPath, ArchCacheName, t.Repo, t.Arch)
}

func (t archTarget) cachedDB(destinationPath string) string {
	return filepath.Join(t.cacheDir(destinationPath), t.Repo+".db")
}

func (t archTarget) cachedFiles(destinationPath string) string {
	return filepath.Join(t.cacheDir(destinationPath), t.Repo+".files")
}

// parseArchSources expands each source line into one target per repository and
// architecture. A line looks like:
//
//	https://geo.mirror.pkgbuild.com/$repo/os/$arch core extra multilib
func parseArchSources(sources []string, architectures []string) []archTarget {
	var targets []archTarget

	for _, source := range sources {
		fields := strings.Fields(source)
		if len(fields) < 2 {
			logError(_t("config error"), ": [server].source:", source)
			continue
		}

		template, repos := fields[0], fields[1:]
		for _, repo := range repos {
			for _, arch := range architectures {
				targets = append(targets, archTarget{Template: template, Repo: repo, Arch: arch})
			}
		}
	}
	return targets
}

type archBackend struct{}

// FetchIndexes downloads every repository database into the local cache.
func (archBackend) FetchIndexes(config *Config) error {
	targets := parseArchSources(config.Server.Source, config.Destination.Arch)
	if len(targets) == 0 {
		return fmt.Errorf("[server].source: %s", _t("it i m a was n sp"))
	}

	type download struct {
		url  string
		dest string
	}

	var downloads []download
	for _, target := range targets {
		downloads = append(downloads, download{target.dbURL(), target.cachedDB(config.Destination.Path)})

		// The .files database is roughly ten times bigger, so it is only
		// fetched when the user actually wants "pacman -F" to work.
		if config.Settings.FilesDatabase {
			downloads = append(downloads, download{target.filesURL(), target.cachedFiles(config.Destination.Path)})
		}
	}

	var failed atomic.Int64
	tasks := make([]func(), 0, len(downloads))

	for _, item := range downloads {
		tasks = append(tasks, func() {
			logDebug(_t("starting d"), item.url)

			// Repository databases carry no checksum of their own, so the
			// configured size heuristic is all that is available.
			if err := downloadFile(item.url, item.dest, fileCheck{}); err != nil {
				logErrorf("%s %s: %v", _t("error d"), item.url, err)
				failed.Add(1)
			}
		})
	}

	runConcurrent(config.Settings.MaxConcurrentDownloads, tasks)

	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%s %d/%d", _t("d failed"), n, len(downloads))
	}
	return nil
}

// archIndex is everything read from the cached databases for one architecture.
type archIndex struct {
	// ByName maps a package name to its entry. The first repository listed
	// wins, which mirrors pacman's own repository precedence.
	ByName map[string]*archEntry
	// Files maps a package directory to the entry read from a .files database.
	Files map[string]*archEntry
}

// loadArchIndex reads the cached databases for one architecture, including the
// .files ones when the configuration asks for them.
func loadArchIndex(config *Config, arch string) (*archIndex, error) {
	return loadArchIndexOpt(config, arch, config.Settings.FilesDatabase)
}

// loadArchIndexOpt is loadArchIndex with the .files databases made optional,
// independently of the configuration. The catalog never wants them: they are
// ten times bigger and carry nothing it reads.
func loadArchIndexOpt(config *Config, arch string, withFiles bool) (*archIndex, error) {
	targets := parseArchSources(config.Server.Source, []string{arch})

	index := &archIndex{
		ByName: map[string]*archEntry{},
		Files:  map[string]*archEntry{},
	}

	found := 0
	for _, target := range targets {
		dbPath := target.cachedDB(config.Destination.Path)
		if !fileExists(dbPath) {
			logErrorf("%s: %s", _t("err r db"), dbPath)
			continue
		}

		entries, err := readArchDB(dbPath, target.Repo, target.baseURL())
		if err != nil {
			return nil, err
		}
		found++

		for _, entry := range entries {
			if _, exists := index.ByName[entry.Name]; !exists {
				index.ByName[entry.Name] = entry
			}
		}

		if !withFiles {
			continue
		}

		filesPath := target.cachedFiles(config.Destination.Path)
		if !fileExists(filesPath) {
			logErrorf("%s: %s", _t("err r db"), filesPath)
			continue
		}

		fileEntries, err := readArchDB(filesPath, target.Repo, target.baseURL())
		if err != nil {
			return nil, err
		}
		for _, entry := range fileEntries {
			if _, exists := index.Files[entry.Dir]; !exists {
				index.Files[entry.Dir] = entry
			}
		}
	}

	if found == 0 {
		return nil, fmt.Errorf("%s %s", _t("no p f"),
			filepath.Join(config.Destination.Path, ArchCacheName))
	}
	return index, nil
}

// catalog builds the format-neutral view the shared resolver works on.
func (index *archIndex) catalog() *catalog {
	cat := newCatalog()

	// Sorted so that, when two packages provide the same virtual name, the
	// choice does not depend on Go's map iteration order.
	for _, name := range slices.Sorted(maps.Keys(index.ByName)) {
		entry := index.ByName[name]
		cat.addPackage(entry.Name,
			archDependencyClauses(entry.Depends),
			archProvidedNames(entry.Provides),
			entry.Groups)
	}
	return cat
}

// Build resolves the requested packages and writes the destination repository.
func (archBackend) Build(config *Config) error {
	var selected, withFiles []*archEntry
	var manifest []manifestEntry

	// Two sets, so they can diverge on demand: seenManifest guards what -dp
	// downloads, seenPublished what the database advertises.
	seenManifest := map[string]bool{}
	seenPublished := map[string]bool{}

	for _, arch := range config.Destination.Arch {
		index, err := loadArchIndex(config, arch)
		if err != nil {
			return &buildError{Code: 3, Err: err}
		}

		names, unresolved := index.catalog().resolveNames(config.Destination.Packages)
		if len(unresolved) > 0 {
			// Silently dropping these is how a mirror ends up subtly incomplete.
			logError(_t("unresolved"), arch, strings.Join(unresolved, " "))
		}

		// The manifest is always the declared closure: -dp downloads only what
		// the user actually asked for.
		for _, name := range names {
			entry := index.ByName[name]
			if entry == nil || seenManifest[entry.Dir] {
				continue
			}
			seenManifest[entry.Dir] = true

			manifest = append(manifest, manifestEntry{
				BaseURL:  entry.BaseURL,
				PoolPath: entry.Filename,
				Size:     entry.Size,
				SHA256:   entry.SHA256,
			})
		}

		// The published database advertises the closure - or, on demand,
		// everything the mirror has, so pacman can ask for a package that is
		// not on disk yet.
		publish := names
		if config.Settings.OnDemand {
			publish = slices.Sorted(maps.Keys(index.ByName))
			logDebugf("%s %s: %d", _t("publishing all"), arch, len(publish))
		}

		for _, name := range publish {
			entry := index.ByName[name]
			if entry == nil || seenPublished[entry.Dir] {
				continue
			}
			seenPublished[entry.Dir] = true

			selected = append(selected, entry)

			// The .files database holds the same packages with one extra
			// member, so it is keyed by the same package directory.
			if entry, ok := index.Files[entry.Dir]; ok {
				withFiles = append(withFiles, entry)
			}
		}

		logDebugf("%s %s: %d", _t("selected"), arch, len(names))
	}

	if len(selected) == 0 {
		return &buildError{Code: 4, Err: errNoPackagesSelected}
	}

	dbPath := filepath.Join(config.Destination.Path, DestinationDistsName+".db")
	if err := writeArchDB(dbPath, selected); err != nil {
		return failBuild(6, "%s %s: %v", _t("error w P"), dbPath, err)
	}
	logDebug(_t("downloaded"), dbPath)

	if config.Settings.FilesDatabase {
		if len(withFiles) == 0 {
			logError(_t("no files db"))
		} else {
			filesPath := filepath.Join(config.Destination.Path, DestinationDistsName+".files")
			if err := writeArchDB(filesPath, withFiles); err != nil {
				return failBuild(6, "%s %s: %v", _t("error w P"), filesPath, err)
			}
			logDebug(_t("downloaded"), filesPath)
		}
	}

	if err := writeManifestEntries(config, manifest); err != nil {
		return &buildError{Code: 14, Err: err}
	}
	return nil
}
