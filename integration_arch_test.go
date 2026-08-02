package main

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Real pacman fixture. core is the same order of magnitude as the Debian
// contrib slice used by the other integration tests: about 130 KB for ~300
// packages, plus a handful of ~2 MB .pkg.tar.zst files.
//
// bash is the seed because it exercises everything at once: it provides "sh",
// it depends on the soname libreadline.so=8-64 which only resolves through
// %PROVIDES%, and it pulls in a real transitive fan-out.
const (
	archFixtureMirror = "https://geo.mirror.pkgbuild.com/$repo/os/$arch"
	archFixtureRepo   = "core"
	archFixtureArch   = "x86_64"
	archFixtureSeed   = "bash"

	// core publishes no %GROUPS%; extra does. Only the group test pays for it.
	archGroupRepo = "extra"
)

func archFixtureSource() string {
	return archFixtureMirror + " " + archFixtureRepo
}

var (
	archCacheOnce sync.Once
	archCachePath string
	archCacheErr  error
)

// archSharedCache downloads the fixture databases once per test binary run,
// caching them on disk across runs, and returns the cache directory.
func archSharedCache(t *testing.T) string {
	t.Helper()
	requireNetwork(t)

	archCacheOnce.Do(func() {
		root := filepath.Join(integrationRoot(), "arch-cache")

		config := defaultConfig()
		config.Server.Type = BackendArch
		config.Server.Source = []string{archFixtureSource()}
		config.Destination.Path = root
		config.Destination.Arch = []string{archFixtureArch}
		config.Destination.Packages = []string{archFixtureSeed}
		config.Settings.FilesDatabase = true

		// Revalidate the on-disk cache cheaply, and put the global back so the
		// value cannot depend on which test ran first.
		originalSkip := SkipDownloadSameSize
		SkipDownloadSameSize = true
		defer func() { SkipDownloadSameSize = originalSkip }()

		if archCacheErr = initHTTPClient(&config); archCacheErr != nil {
			return
		}
		if archCacheErr = (archBackend{}).FetchIndexes(&config); archCacheErr != nil {
			return
		}
		archCachePath = filepath.Join(root, ArchCacheName)
	})

	if archCacheErr != nil {
		t.Fatalf("could not fetch the pacman fixture databases: %v", archCacheErr)
	}
	return archCachePath
}

// newArchRepo returns a config pointing at a fresh per-test directory seeded
// with a copy of the shared database cache.
func newArchRepo(t *testing.T, packages ...string) *Config {
	t.Helper()

	cache := archSharedCache(t)

	if len(packages) == 0 {
		packages = []string{archFixtureSeed}
	}

	dest := filepath.Join(integrationRoot(), "arch-runs", t.Name())
	if err := os.RemoveAll(dest); err != nil {
		t.Fatalf("clean %s: %v", dest, err)
	}
	if err := copyTree(cache, filepath.Join(dest, ArchCacheName)); err != nil {
		t.Fatalf("seed the database cache: %v", err)
	}

	config := defaultConfig()
	config.Server.Type = BackendArch
	config.Server.Source = []string{archFixtureSource()}
	config.Destination.Path = dest
	config.Destination.Arch = []string{archFixtureArch}
	config.Destination.Packages = packages
	config.Settings.FilesDatabase = true
	return &config
}

func archCachedDB(config *Config) string {
	return filepath.Join(config.Destination.Path, ArchCacheName,
		archFixtureRepo, archFixtureArch, archFixtureRepo+".db")
}

func archGeneratedDB(config *Config) string {
	return filepath.Join(config.Destination.Path, DestinationDistsName+".db")
}

// archReadUpstream parses the cached fixture database.
func archReadUpstream(t *testing.T, config *Config) []*archEntry {
	t.Helper()

	entries, err := readArchDB(archCachedDB(config), archFixtureRepo, "https://example.invalid")
	if err != nil {
		t.Fatalf("readArchDB: %v", err)
	}
	if len(entries) < 100 {
		t.Fatalf("the fixture database holds %d packages, too few to be %s", len(entries), archFixtureRepo)
	}
	return entries
}

func TestIntegrationArchDatabaseParsesRealPackages(t *testing.T) {
	config := newArchRepo(t)
	entries := archReadUpstream(t, config)

	provides, groups, sonames := 0, 0, 0

	for _, entry := range entries {
		if entry.Name == "" {
			t.Errorf("%s: parsed with no %%NAME%%", entry.Dir)
			continue
		}
		if entry.Version == "" {
			t.Errorf("%s: no %%VERSION%%", entry.Name)
		}
		if entry.Arch == "" {
			t.Errorf("%s: no %%ARCH%%", entry.Name)
		}
		// Every real package must be downloadable and verifiable.
		if entry.Filename == "" {
			t.Errorf("%s: no %%FILENAME%%", entry.Name)
		}
		if strings.Contains(entry.Filename, "/") {
			t.Errorf("%s: %%FILENAME%% %q contains a path, but pacman repositories are flat",
				entry.Name, entry.Filename)
		}
		if entry.Size <= 0 {
			t.Errorf("%s: %%CSIZE%% is %d", entry.Name, entry.Size)
		}
		if len(entry.SHA256) != 64 {
			t.Errorf("%s: %%SHA256SUM%% is %q", entry.Name, entry.SHA256)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			t.Errorf("%s: %%SHA256SUM%% is not hex: %v", entry.Name, err)
		}

		if len(entry.Provides) > 0 {
			provides++
		}
		if len(entry.Groups) > 0 {
			groups++
		}
		for _, dep := range entry.Depends {
			if strings.Contains(dep, ".so") {
				sonames++
			}
		}
	}

	// Guard against the loop above passing vacuously if the parser ever stops
	// reading the optional fields.
	if provides == 0 {
		t.Error("not one package declared %PROVIDES%, so the provides path was never exercised")
	}
	if sonames == 0 {
		t.Error("not one soname dependency was parsed")
	}
	t.Logf("%d packages, %d with %%PROVIDES%%, %d with %%GROUPS%%, %d soname dependencies",
		len(entries), provides, groups, sonames)
}

func TestIntegrationArchResolvesThroughProvides(t *testing.T) {
	config := newArchRepo(t)

	index, err := loadArchIndex(config, archFixtureArch)
	if err != nil {
		t.Fatalf("loadArchIndex: %v", err)
	}

	seed := index.ByName[archFixtureSeed]
	if seed == nil {
		t.Fatalf("%s is not in the fixture database", archFixtureSeed)
	}

	// Find a soname the seed depends on, rather than hardcoding one: the
	// version in libreadline.so=8-64 moves with upstream.
	var soname string
	for _, dep := range seed.Depends {
		if strings.Contains(dep, ".so") {
			soname = archDependencyName(dep)
			break
		}
	}
	if soname == "" {
		t.Skipf("%s no longer has a soname dependency: %v", archFixtureSeed, seed.Depends)
	}

	// The soname is not a package name, so only %PROVIDES% can satisfy it.
	if _, isPackage := index.ByName[soname]; isPackage {
		t.Skipf("%s is a real package name in this snapshot", soname)
	}

	selected, unresolved := index.catalog().resolveNames([]string{archFixtureSeed})

	if slices.Contains(unresolved, soname) {
		t.Errorf("%s was reported unresolved even though a package provides it", soname)
	}

	// Whoever provides it must have been pulled in.
	var providers []string
	for name, entry := range index.ByName {
		if slices.Contains(archProvidedNames(entry.Provides), soname) {
			providers = append(providers, name)
		}
	}
	if len(providers) == 0 {
		t.Fatalf("nothing in the fixture provides %s", soname)
	}

	satisfied := false
	for _, provider := range providers {
		if slices.Contains(selected, provider) {
			satisfied = true
			break
		}
	}
	if !satisfied {
		t.Errorf("%s depends on %s, provided by %v, but none of them was selected",
			archFixtureSeed, soname, providers)
	}
	t.Logf("%s -> %s -> %v", archFixtureSeed, soname, providers)
}

// The same invariant that protects the Debian side: nothing may be dropped in
// silence. Every dependency is either selected or reported.
func TestIntegrationArchEveryDependencyIsAccountedFor(t *testing.T) {
	config := newArchRepo(t)

	index, err := loadArchIndex(config, archFixtureArch)
	if err != nil {
		t.Fatalf("loadArchIndex: %v", err)
	}

	selected, unresolved := index.catalog().resolveNames([]string{archFixtureSeed})
	if len(selected) < 2 {
		t.Fatalf("selected %v, want a real fan-out", selected)
	}

	isSelected := make(map[string]bool, len(selected))
	for _, name := range selected {
		isSelected[name] = true
	}
	isReported := make(map[string]bool, len(unresolved))
	for _, name := range unresolved {
		isReported[name] = true
	}

	// A dependency can be satisfied by a package with another name.
	providers := map[string][]string{}
	for name, entry := range index.ByName {
		for _, virtual := range archProvidedNames(entry.Provides) {
			providers[virtual] = append(providers[virtual], name)
		}
	}

	checked := 0
	for _, name := range selected {
		for _, dep := range index.ByName[name].Depends {
			wanted := archDependencyName(dep)
			if wanted == "" {
				continue
			}
			checked++

			if isSelected[wanted] || isReported[wanted] {
				continue
			}

			satisfied := false
			for _, provider := range providers[wanted] {
				if isSelected[provider] {
					satisfied = true
					break
				}
			}
			if !satisfied {
				t.Errorf("%s: dependency %q was dropped silently: it is not selected, not provided by anything selected, and not reported as unresolved",
					name, dep)
			}
		}
	}

	if checked == 0 {
		t.Fatalf("checked 0 dependencies over %d selected packages", len(selected))
	}
	t.Logf("checked %d dependencies over %d selected packages, %d unresolved",
		checked, len(selected), len(unresolved))
}

func TestIntegrationArchBuildCopiesMembersVerbatim(t *testing.T) {
	config := newArchRepo(t)

	if err := (archBackend{}).Build(config); err != nil {
		t.Fatalf("Build: %v", err)
	}

	upstream := map[string]*archEntry{}
	for _, entry := range archReadUpstream(t, config) {
		upstream[entry.Dir] = entry
	}

	generated, err := readArchDB(archGeneratedDB(config), archFixtureRepo, "https://example.invalid")
	if err != nil {
		t.Fatalf("readArchDB(generated): %v", err)
	}
	if len(generated) == 0 {
		t.Fatal("the generated database holds no packages")
	}
	if len(generated) >= len(upstream) {
		t.Errorf("the generated database holds %d of the %d upstream packages: seeding one package should select a subset",
			len(generated), len(upstream))
	}

	compared := 0
	for _, entry := range generated {
		source, ok := upstream[entry.Dir]
		if !ok {
			t.Errorf("%s is in the generated database but not upstream", entry.Dir)
			continue
		}

		// The point of copying members instead of re-serialising desc: no
		// field can be lost, including %PGPSIG% and anything tinyrepo does
		// not model.
		got := archMemberData(entry, "desc")
		want := archMemberData(source, "desc")
		if got == "" {
			t.Errorf("%s: the generated database has no desc member", entry.Dir)
			continue
		}
		if got != want {
			t.Errorf("%s: the desc member was modified on the way out", entry.Dir)
			continue
		}
		compared++
	}

	if compared == 0 {
		t.Fatal("compared no desc members")
	}
	t.Logf("%d packages, all desc members byte for byte identical to upstream", compared)
}

func TestIntegrationArchBuildWritesFilesDatabase(t *testing.T) {
	config := newArchRepo(t)

	if err := (archBackend{}).Build(config); err != nil {
		t.Fatalf("Build: %v", err)
	}

	filesPath := filepath.Join(config.Destination.Path, DestinationDistsName+".files")
	if !fileExists(filesPath) {
		t.Fatalf("%s was not generated", filesPath)
	}

	entries, err := readArchDB(filesPath, archFixtureRepo, "https://example.invalid")
	if err != nil {
		t.Fatalf("readArchDB(files): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the .files database holds no packages")
	}

	withFileList := 0
	for _, entry := range entries {
		if archMemberData(entry, "desc") == "" {
			t.Errorf("%s: the .files database must also carry desc", entry.Dir)
		}

		files := archMemberData(entry, "files")
		if files == "" {
			continue
		}
		if !strings.HasPrefix(files, "%FILES%") {
			t.Errorf("%s: the files member does not start with %%FILES%%", entry.Dir)
		}
		withFileList++
	}

	if withFileList == 0 {
		t.Fatal("not one package carries a file list, so 'pacman -F' would find nothing")
	}
	t.Logf("%d packages, %d with a file list", len(entries), withFileList)
}

func TestIntegrationArchManifestAndPoolDownload(t *testing.T) {
	// A small leaf package keeps this test to a couple of MB.
	config := newArchRepo(t)
	config.Settings.FilesDatabase = false

	index, err := loadArchIndex(config, archFixtureArch)
	if err != nil {
		t.Fatalf("loadArchIndex: %v", err)
	}

	// Pick the smallest package with no dependencies, so the download stays
	// tiny whatever upstream currently ships.
	var seed *archEntry
	for _, entry := range index.ByName {
		if len(entry.Depends) != 0 || entry.Size <= 0 {
			continue
		}
		if seed == nil || entry.Size < seed.Size || (entry.Size == seed.Size && entry.Name < seed.Name) {
			seed = entry
		}
	}
	if seed == nil {
		t.Skip("no dependency-free package in the fixture")
	}

	config.Destination.Packages = []string{seed.Name}
	if err := (archBackend{}).Build(config); err != nil {
		t.Fatalf("Build: %v", err)
	}

	entries, err := readManifest(manifestPath(config))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the manifest is empty")
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.BaseURL, "https://") {
			t.Errorf("%s: BaseURL is %q", entry.PoolPath, entry.BaseURL)
		}
		if strings.Contains(entry.PoolPath, "/") {
			t.Errorf("%s: a pacman pool path must be a bare file name", entry.PoolPath)
		}
		if entry.Size <= 0 || len(entry.SHA256) != 64 {
			t.Errorf("%s: size %d, sha256 %q", entry.PoolPath, entry.Size, entry.SHA256)
		}
	}

	if err := downloadPool(config); err != nil {
		t.Fatalf("downloadPool: %v", err)
	}

	for _, entry := range entries {
		path := filepath.Join(config.Destination.Path, entry.PoolPath)

		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s was not downloaded: %v", entry.PoolPath, err)
			continue
		}
		if info.Size() != entry.Size {
			t.Errorf("%s is %d bytes, the manifest says %d", entry.PoolPath, info.Size(), entry.Size)
		}

		_, sha256Hex, _, err := hashFile(path)
		if err != nil {
			t.Fatalf("hashFile %s: %v", path, err)
		}
		if !strings.EqualFold(sha256Hex, entry.SHA256) {
			t.Errorf("%s: sha256 on disk is %s, the manifest says %s", entry.PoolPath, sha256Hex, entry.SHA256)
		}

		// A real .pkg.tar.zst starts with the zstd magic number.
		data, err := os.ReadFile(path)
		if err != nil || len(data) < 4 {
			t.Fatalf("read %s: %v", path, err)
		}
		if magic := data[:4]; magic[0] != 0x28 || magic[1] != 0xB5 || magic[2] != 0x2F || magic[3] != 0xFD {
			t.Errorf("%s starts with % x, want the zstd magic 28 b5 2f fd", entry.PoolPath, magic)
		}
	}
	t.Logf("downloaded and verified %d package(s), seeded with %s", len(entries), seed.Name)
}

// archGroupIndex loads a repository that actually publishes %GROUPS%.
//
// core does not have any, so the group test needs "extra". Its database is
// about 8.7 MB rather than core's 130 KB, so it is fetched lazily, only by the
// test that needs it, and cached on disk like every other fixture.
func archGroupIndex(t *testing.T) *archIndex {
	t.Helper()
	requireNetwork(t)

	root := filepath.Join(integrationRoot(), "arch-groups")

	config := defaultConfig()
	config.Server.Type = BackendArch
	config.Server.Source = []string{archFixtureMirror + " " + archGroupRepo}
	config.Destination.Path = root
	config.Destination.Arch = []string{archFixtureArch}
	config.Settings.FilesDatabase = false

	originalSkip := SkipDownloadSameSize
	SkipDownloadSameSize = true
	defer func() { SkipDownloadSameSize = originalSkip }()

	if err := (archBackend{}).FetchIndexes(&config); err != nil {
		t.Fatalf("could not fetch the %s database: %v", archGroupRepo, err)
	}

	index, err := loadArchIndex(&config, archFixtureArch)
	if err != nil {
		t.Fatalf("loadArchIndex(%s): %v", archGroupRepo, err)
	}
	return index
}

func TestIntegrationArchGroupSeedExpands(t *testing.T) {
	cat := archGroupIndex(t).catalog()

	if len(cat.Groups) == 0 {
		t.Skipf("no %%GROUPS%% in the %s snapshot", archGroupRepo)
	}

	// Take the smallest group with more than one member, discovered rather
	// than hardcoded: which groups exist moves with upstream.
	group, members := "", []string(nil)
	for name, names := range cat.Groups {
		if len(names) < 2 {
			continue
		}
		if members == nil || len(names) < len(members) || (len(names) == len(members) && name < group) {
			group, members = name, names
		}
	}
	if group == "" {
		t.Skipf("no %s group has more than one member", archGroupRepo)
	}

	selected, _ := cat.resolveNames([]string{group})

	for _, member := range members {
		if !slices.Contains(selected, member) {
			t.Errorf("seeding the group %q did not select its member %q", group, member)
		}
	}
	if len(selected) < len(members) {
		t.Errorf("selected %d packages for a group of %d members", len(selected), len(members))
	}
	t.Logf("group %q expanded to %d members, %d packages selected", group, len(members), len(selected))
}

func TestIntegrationArchUnknownSeedIsReported(t *testing.T) {
	config := newArchRepo(t, "tinyrepo-no-such-package-9f3b")

	err := (archBackend{}).Build(config)
	if err == nil {
		t.Fatal("Build accepted a seed that does not exist")
	}

	var failure *buildError
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a buildError carrying an exit code", err)
	}
	if failure.Code != 4 {
		t.Errorf("exit code = %d, want 4 (no packages were selected)", failure.Code)
	}
}

// archMemberData returns the contents of a named member of an entry.
func archMemberData(entry *archEntry, name string) string {
	for _, member := range entry.Members {
		if strings.HasSuffix(member.Header.Name, "/"+name) {
			return string(member.Data)
		}
	}
	return ""
}
