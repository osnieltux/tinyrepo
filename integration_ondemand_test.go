package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// On-demand mode against the real archives.
// See integration_test.go for the shared fixture and scaffolding.

// withRawStanzas turns on the parser flag that main.go sets from
// [settings].onDemand, and puts it back afterwards so the value never depends on
// which test ran first.
func withRawStanzas(t *testing.T) {
	t.Helper()

	original := KeepRawStanzas
	KeepRawStanzas = true
	t.Cleanup(func() { KeepRawStanzas = original })
}

// onDemandRepo builds a real on-demand repository from the cached fixture.
func onDemandRepo(t *testing.T, packages ...string) *Config {
	t.Helper()
	withRawStanzas(t)

	config := newRepo(t, packages...)
	config.Settings.OnDemand = true

	if err := (debianBackend{}).Build(config); err != nil {
		t.Fatalf("Build(onDemand): %v", err)
	}
	return config
}

// mustLocalPath resolves an entry's path inside the repository, failing the
// test if it would escape it.
func mustLocalPath(t *testing.T, entry manifestEntry, root string) string {
	t.Helper()

	path, err := entry.localPath(root)
	if err != nil {
		t.Fatalf("localPath(%s): %v", entry.PoolPath, err)
	}
	return path
}

func generatedPackagesPath(config *Config) string {
	return filepath.Join(config.Destination.Path, "dists", DestinationDistsName,
		DestinationComponent, "binary-"+fixtureArch, "Packages")
}

func countStanzas(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Package: ") {
			count++
		}
	}
	return count
}

// The point of the mode: the index has to advertise everything the mirror has,
// or a client can never ask for a package we do not hold yet. The manifest, on
// the other hand, must stay the declared closure.
func TestIntegrationOnDemandPublishesFullCatalog(t *testing.T) {
	requireNetwork(t)

	config := onDemandRepo(t)

	upstream := countStanzas(t, cachedIndexPath(config))
	published := countStanzas(t, generatedPackagesPath(config))

	if published != upstream {
		t.Errorf("the published index has %d packages, the cached mirror index has %d: on demand must publish all of them",
			published, upstream)
	}

	entries, err := readManifest(manifestPath(config))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the manifest is empty, so -dp would download nothing")
	}
	if len(entries) >= published {
		t.Errorf("the manifest holds %d entries and the index %d: the manifest must stay the declared closure",
			len(entries), published)
	}

	t.Logf("published %d packages, manifest holds %d", published, len(entries))
}

// Publishing the whole archive through writeStanza would drop Conflicts,
// Replaces and Multi-Arch, which apt actually acts on. Republishing the stanza
// verbatim is what keeps the archive installable.
func TestIntegrationOnDemandPublishesStanzasVerbatim(t *testing.T) {
	requireNetwork(t)

	config := onDemandRepo(t)

	cached, err := os.ReadFile(cachedIndexPath(config))
	if err != nil {
		t.Fatalf("read the cached index: %v", err)
	}
	published, err := os.ReadFile(generatedPackagesPath(config))
	if err != nil {
		t.Fatalf("read the published index: %v", err)
	}

	// Fields the struct has no room for, so re-serialising would lose them.
	for _, field := range []string{"Conflicts:", "Replaces:", "Multi-Arch:", "Recommends:"} {
		upstreamHas := strings.Contains(string(cached), "\n"+field)
		if !upstreamHas {
			t.Logf("the fixture slice carries no %s, nothing to check", field)
			continue
		}
		if !strings.Contains(string(published), "\n"+field) {
			t.Errorf("%s is in the mirror index but not in the one we publish: apt would plan installs against a field we deleted", field)
		}
	}
}

// The .xz variant costs a minute or more for a 50 MB index in pure Go, so on
// demand it is skipped. Release must then not promise it, or apt fails.
func TestIntegrationOnDemandReleaseMatchesWhatExists(t *testing.T) {
	requireNetwork(t)

	config := onDemandRepo(t)

	releasePath := filepath.Join(config.Destination.Path, "dists", DestinationDistsName, "Release")
	release, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatalf("read Release: %v", err)
	}

	if strings.Contains(string(release), "Packages.xz") {
		t.Error("Release names Packages.xz, which on-demand mode does not generate")
	}
	if !strings.Contains(string(release), "Packages.gz") {
		t.Error("Release does not name Packages.gz, so apt has no compressed index to fetch")
	}

	// Every file Release lists has to exist, or apt reports a hash mismatch.
	for _, line := range strings.Split(string(release), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !strings.HasPrefix(fields[2], DestinationComponent+"/") {
			continue
		}
		named := filepath.Join(config.Destination.Path, "dists", DestinationDistsName,
			filepath.FromSlash(fields[2]))
		if !fileExists(named) {
			t.Errorf("Release lists %s, which does not exist", fields[2])
		}
	}
}

// The end-to-end behaviour, against the real mirror: ask for a package that was
// never declared and is not on disk, and get it.
func TestIntegrationOnDemandServesRealPackage(t *testing.T) {
	requireNetwork(t)

	config := onDemandRepo(t)

	backend := debianBackend{}
	od, err := newOnDemand(t.Context(), config, backend)
	if err != nil {
		t.Fatalf("newOnDemand: %v", err)
	}
	defer od.stop()

	server := httptest.NewServer(repoHandler{root: config.Destination.Path, onDemand: od})
	defer server.Close()

	// Pick a real package that the declared closure did not bring in, and the
	// smallest one available so the test stays cheap.
	declared := map[string]bool{}
	entries, err := readManifest(manifestPath(config))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	for _, entry := range entries {
		declared[entry.PoolPath] = true
	}

	var target manifestEntry
	for path, entry := range od.cat.Files {
		if declared[path] || entry.SHA256 == "" || entry.Size <= 0 {
			continue
		}
		if target.PoolPath == "" || entry.Size < target.Size {
			target = entry
		}
	}
	if target.PoolPath == "" {
		t.Skip("every package in the fixture slice is already declared")
	}

	onDisk := mustLocalPath(t, target, config.Destination.Path)
	if fileExists(onDisk) {
		t.Fatalf("%s is already on disk, so this would not test a miss", target.PoolPath)
	}

	t.Logf("requesting %s (%d bytes), which -dp never downloaded", target.PoolPath, target.Size)

	response, err := server.Client().Get(server.URL + "/" + target.PoolPath)
	if err != nil {
		t.Fatalf("GET %s: %v", target.PoolPath, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}

	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, target.SHA256) {
		t.Errorf("the body served has SHA256 %s, the index publishes %s", got, target.SHA256)
	}
	if int64(len(body)) != target.Size {
		t.Errorf("served %d bytes, the index publishes %d", len(body), target.Size)
	}

	// And it must have been cached, or every request would refetch it.
	if !fileExists(onDisk) {
		t.Fatalf("%s was served but not cached", target.PoolPath)
	}
	cached, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if string(cached) != string(body) {
		t.Error("the cached copy differs from what was served")
	}

	// The second request is a hit, which is where byte ranges come from.
	ranged, err := http.NewRequest(http.MethodGet, server.URL+"/"+target.PoolPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	ranged.Header.Set("Range", "bytes=0-9")

	hit, err := server.Client().Do(ranged)
	if err != nil {
		t.Fatal(err)
	}
	defer hit.Body.Close()

	if hit.StatusCode != http.StatusPartialContent {
		t.Errorf("a cached package answered a Range with %d, want 206", hit.StatusCode)
	}
}

// The pacman side of the same thing.
func TestIntegrationArchOnDemandPublishesFullCatalog(t *testing.T) {
	requireNetwork(t)

	config := newArchRepo(t)
	config.Settings.OnDemand = true
	// The .files database of a full repo is large and adds nothing here.
	config.Settings.FilesDatabase = false

	if err := (archBackend{}).Build(config); err != nil {
		t.Fatalf("Build(onDemand): %v", err)
	}

	upstream := archReadUpstream(t, config)

	generated, err := readArchDB(archGeneratedDB(config), DestinationDistsName, "")
	if err != nil {
		t.Fatalf("readArchDB: %v", err)
	}

	if len(generated) != len(upstream) {
		t.Errorf("the published database holds %d packages, the mirror's holds %d",
			len(generated), len(upstream))
	}

	entries, err := readManifest(manifestPath(config))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(entries) == 0 || len(entries) >= len(generated) {
		t.Errorf("the manifest holds %d entries and the database %d: the manifest must stay the declared closure",
			len(entries), len(generated))
	}

	t.Logf("published %d packages, manifest holds %d", len(generated), len(entries))
}

func TestIntegrationArchOnDemandServesRealPackage(t *testing.T) {
	requireNetwork(t)

	config := newArchRepo(t)
	config.Settings.OnDemand = true
	config.Settings.FilesDatabase = false

	if err := (archBackend{}).Build(config); err != nil {
		t.Fatalf("Build(onDemand): %v", err)
	}

	od, err := newOnDemand(t.Context(), config, archBackend{})
	if err != nil {
		t.Fatalf("newOnDemand: %v", err)
	}
	defer od.stop()

	server := httptest.NewServer(repoHandler{root: config.Destination.Path, onDemand: od})
	defer server.Close()

	declared := map[string]bool{}
	entries, err := readManifest(manifestPath(config))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	for _, entry := range entries {
		declared[entry.PoolPath] = true
	}

	var target manifestEntry
	for path, entry := range od.cat.Files {
		if declared[path] || entry.SHA256 == "" || entry.Size <= 0 {
			continue
		}
		if target.PoolPath == "" || entry.Size < target.Size {
			target = entry
		}
	}
	if target.PoolPath == "" {
		t.Skip("every package in the fixture database is already declared")
	}

	t.Logf("requesting %s (%d bytes)", target.PoolPath, target.Size)

	response, err := server.Client().Get(server.URL + "/" + target.PoolPath)
	if err != nil {
		t.Fatalf("GET %s: %v", target.PoolPath, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, target.SHA256) {
		t.Errorf("the body served has SHA256 %s, the database publishes %s", got, target.SHA256)
	}

	// Every pacman package is a zstd stream; anything else means we served
	// something that is not the package.
	if len(body) < 4 || body[0] != 0x28 || body[1] != 0xB5 || body[2] != 0x2F || body[3] != 0xFD {
		t.Errorf("the body does not start with the zstd magic number")
	}

	if !fileExists(mustLocalPath(t, target, config.Destination.Path)) {
		t.Error("the package was served but not cached")
	}
}

// -cl against a real repository: what config.toml declares survives, what
// arrived on demand does not, and the repository itself is untouched.
func TestIntegrationCleanupRemovesOnDemandExtras(t *testing.T) {
	requireNetwork(t)

	config := onDemandRepo(t)

	if err := downloadPool(config); err != nil {
		t.Fatalf("downloadPool: %v", err)
	}

	entries, err := readManifest(manifestPath(config))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the manifest is empty, so there is nothing to protect")
	}

	// Stand in for a package fetched on demand: a real pool path from the
	// catalog that the declared closure never asked for.
	declared := map[string]bool{}
	for _, entry := range entries {
		declared[entry.PoolPath] = true
	}

	extra := ""
	backend := debianBackend{}
	cat, err := backend.Catalog(config)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	for path := range cat.Files {
		if !declared[path] {
			extra = path
			break
		}
	}
	if extra == "" {
		t.Skip("every package in the fixture slice is declared")
	}

	extraPath := filepath.Join(config.Destination.Path, filepath.FromSlash(extra))
	if err := os.MkdirAll(filepath.Dir(extraPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extraPath, []byte("fetched on demand"), 0644); err != nil {
		t.Fatal(err)
	}

	// And a leftover from an interrupted download.
	stale := extraPath + TempSuffix
	if err := os.WriteFile(stale, []byte("half"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := cleanRepo(config, backend); err != nil {
		t.Fatalf("cleanRepo: %v", err)
	}

	if fileExists(extraPath) {
		t.Errorf("%s survived, but config.toml never asked for it", extra)
	}
	if fileExists(stale) {
		t.Error("an interrupted download was left behind")
	}

	for _, entry := range entries {
		if !fileExists(mustLocalPath(t, entry, config.Destination.Path)) {
			t.Errorf("%s was deleted, but it is in the manifest", entry.PoolPath)
		}
	}

	// The repository itself has to come through untouched.
	for _, name := range []string{
		filepath.Join("dists", DestinationDistsName, "Release"),
		filepath.Join("dists", DestinationDistsName, DestinationComponent, "binary-"+fixtureArch, "Packages"),
		filepath.Join(ManifestDirName, ManifestFileName),
		DistsCacheName,
	} {
		if !fileExists(filepath.Join(config.Destination.Path, name)) {
			t.Errorf("-cl deleted %s, which is part of the repository", name)
		}
	}
}

// The prefetch is what makes the request after a miss a hit.
func TestIntegrationOnDemandPrefetchesDependencies(t *testing.T) {
	requireNetwork(t)

	config := onDemandRepo(t)

	od, err := newOnDemand(t.Context(), config, debianBackend{})
	if err != nil {
		t.Fatalf("newOnDemand: %v", err)
	}
	defer od.stop()

	server := httptest.NewServer(repoHandler{root: config.Destination.Path, onDemand: od})
	defer server.Close()

	// A package with a dependency that is also in the fixture slice.
	var target, dependency manifestEntry
	for path := range od.cat.Files {
		closure := od.cat.closure(path)
		if len(closure) == 0 {
			continue
		}
		entry := od.cat.Files[path]
		if entry.Size <= 0 || entry.Size > 200_000 {
			continue
		}
		for _, candidate := range closure {
			if candidate.Size > 0 && candidate.Size < 200_000 {
				target, dependency = entry, candidate
				break
			}
		}
		if target.PoolPath != "" {
			break
		}
	}
	if target.PoolPath == "" {
		t.Skip("no small package with a small dependency in the fixture slice")
	}

	t.Logf("requesting %s, expecting %s to be prefetched", target.PoolPath, dependency.PoolPath)

	response, err := server.Client().Get(server.URL + "/" + target.PoolPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()

	dependencyPath := mustLocalPath(t, dependency, config.Destination.Path)
	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		if fileExists(dependencyPath) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("%s was never prefetched after %s was requested", dependency.PoolPath, target.PoolPath)
}

// -di has to verify what it caches against the checksum the mirror publishes in
// its own Release. A tampered index poisons everything built afterwards.
func TestIntegrationIndexIsVerifiedAgainstRelease(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)

	release, err := fetchReleaseIndex(fixtureMirror, fixtureDist)
	if err != nil {
		t.Skipf("%s publishes no readable Release: %v", fixtureMirror, err)
	}

	want, ok := release.check(fixtureComponent, fixtureArch, "Packages")
	if !ok {
		t.Fatalf("the real Release of %s does not list %s/binary-%s/Packages",
			fixtureDist, fixtureComponent, fixtureArch)
	}

	cached := cachedIndexPath(config)
	if !fileExists(cached) {
		t.Fatalf("%s was never cached", cached)
	}

	// newRepo seeds from the shared cache, which -di produced. If the two
	// disagree, verification is not actually happening.
	if !want.matchesLocal(cached) {
		t.Errorf("the cached index does not match the SHA256 %s published in Release", want.SHA256)
	}
	t.Logf("verified %s against Release (%d bytes)", cached, want.Size)
}

// A mirror with no Release must warn and carry on, not abort the build.
func TestIntegrationMissingReleaseIsAWarning(t *testing.T) {
	requireNetwork(t)

	targets := []indexTarget{{
		BaseURL:   fixtureMirror,
		Dist:      "tinyrepo-no-such-suite",
		Component: "main",
		Arch:      fixtureArch,
	}}

	releases := fetchReleaseIndexes(targets)
	if len(releases) != 1 {
		t.Fatalf("read %d releases, want one entry per dist", len(releases))
	}
	if releases["tinyrepo-no-such-suite"] != nil {
		t.Error("a suite that does not exist produced a Release")
	}
}
