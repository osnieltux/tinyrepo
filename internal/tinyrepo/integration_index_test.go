package tinyrepo

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Index acquisition over the real network.
// See integration_test.go for the shared fixture and scaffolding.

// indexHeadStatus performs a HEAD request with the shared client and returns
// the status code, so a test can prove a URL composed by the code under test
// really resolves upstream.
func indexHeadStatus(t *testing.T, url string) int {
	t.Helper()

	request, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		t.Fatalf("HEAD %s: %v", url, err)
	}

	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatalf("HEAD %s: %v", url, err)
	}
	defer response.Body.Close()

	return response.StatusCode
}

// The mirror publishes only compressed indexes, so downloadIndex has to leave
// both the file it fetched and the file it decompressed behind: -ci reads the
// plain one and the next -di revalidates the compressed one.
func TestIntegrationDownloadIndexProducesBothVariants(t *testing.T) {
	requireNetwork(t)

	cacheDir := filepath.Join(sharedCache(t), fixtureDist, fixtureComponent, "binary-"+fixtureArch)

	compressedName := PackagesExtensionPreference[0]
	if filepath.Ext(compressedName) == "" {
		t.Fatalf("PackagesExtensionPreference[0] = %q, want a compressed variant", compressedName)
	}
	compressed := filepath.Join(cacheDir, compressedName)
	plain := filepath.Join(cacheDir, "Packages")

	for _, path := range []string{compressed, plain} {
		if !fileExists(path) {
			t.Fatalf("downloadIndex did not leave %s behind", path)
		}
	}

	_, _, compressedSize, err := hashFile(compressed)
	if err != nil {
		t.Fatalf("hashFile %s: %v", compressed, err)
	}
	_, plainSHA, plainSize, err := hashFile(plain)
	if err != nil {
		t.Fatalf("hashFile %s: %v", plain, err)
	}

	// A truncated download or a saved error page would be far smaller than any
	// real index; the exact sizes are upstream's business.
	if compressedSize < 1024 {
		t.Errorf("%s is %d bytes, too small to be a real index", compressed, compressedSize)
	}
	if plainSize < 2*compressedSize {
		t.Errorf("%s is %d bytes and %s is %d bytes: the plain index should be several times larger than the compressed one",
			plain, plainSize, compressed, compressedSize)
	}

	// The real invariant: the plain file must be exactly what decompressing the
	// compressed file produces. Decompress a copy so the shared cache is left
	// untouched.
	replicaDir := t.TempDir()
	replica := filepath.Join(replicaDir, compressedName)

	data, err := os.ReadFile(compressed)
	if err != nil {
		t.Fatalf("read %s: %v", compressed, err)
	}
	if err := os.WriteFile(replica, data, 0644); err != nil {
		t.Fatalf("write %s: %v", replica, err)
	}
	if err := decompress(replica); err != nil {
		t.Fatalf("decompress %s: %v", replica, err)
	}

	replicaPlain := filepath.Join(replicaDir, "Packages")
	_, replicaSHA, _, err := hashFile(replicaPlain)
	if err != nil {
		t.Fatalf("hashFile %s: %v", replicaPlain, err)
	}
	if replicaSHA != plainSHA {
		t.Errorf("%s does not match the decompression of %s (sha256 %s != %s)",
			plain, compressed, plainSHA, replicaSHA)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("read dir %s: %v", cacheDir, err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tinyrepo.tmp") {
			t.Errorf("temporary file left behind in %s: %s", cacheDir, entry.Name())
		}
	}
}

// The mirror serves .xz and .gz, so a .gz in the cache would mean the
// preference order was ignored and a bigger file was fetched for nothing.
func TestIntegrationDownloadIndexPrefersTheFirstVariant(t *testing.T) {
	requireNetwork(t)

	cacheDir := filepath.Join(sharedCache(t), fixtureDist, fixtureComponent, "binary-"+fixtureArch)

	preferred := PackagesExtensionPreference[0]
	if preferred != "Packages.xz" {
		t.Fatalf("PackagesExtensionPreference[0] = %q, want %q: this test assumes the xz variant is preferred and must be rewritten if that changes",
			preferred, "Packages.xz")
	}
	if !fileExists(filepath.Join(cacheDir, preferred)) {
		t.Fatalf("the preferred variant %s was not fetched into %s", preferred, cacheDir)
	}

	remoteDir := fixtureMirror + "/dists/" + fixtureDist + "/" + fixtureComponent + "/binary-" + fixtureArch

	// Without this probe the assertion below would also pass on a mirror that
	// simply does not publish a second variant.
	fallback := "Packages.gz"
	if status := indexHeadStatus(t, remoteDir+"/"+fallback); status != http.StatusOK {
		t.Skipf("%s/%s returned %d, the mirror no longer serves a second variant to prefer against",
			remoteDir, fallback, status)
	}
	if path := filepath.Join(cacheDir, fallback); fileExists(path) {
		t.Errorf("%s exists: fetchTargetIndex kept going past %s instead of stopping at the first variant the mirror serves"+
			" (if this is a leftover from an older run, delete %s and try again)",
			path, preferred, integrationRoot())
	}

	// And what landed on disk is really xz, not another variant saved under the
	// preferred name.
	file, err := os.Open(filepath.Join(cacheDir, preferred))
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Join(cacheDir, preferred), err)
	}
	defer file.Close()

	header := make([]byte, 6)
	if _, err := io.ReadFull(file, header); err != nil {
		t.Fatalf("read the header of %s: %v", preferred, err)
	}
	if xzMagic := []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}; !bytes.Equal(header, xzMagic) {
		t.Errorf("%s starts with % x, want the xz magic % x", preferred, header, xzMagic)
	}
}

// url_base.txt is how -ci later learns which mirror a cached index came from,
// so it must hold the bare URL: a trailing slash or newline would produce
// "http://mirror//pool/..." download URLs.
func TestIntegrationDownloadIndexRecordsMirrorURL(t *testing.T) {
	requireNetwork(t)

	path := filepath.Join(sharedCache(t), fixtureDist, DistsCacheNameUrlBase)
	if got := readFile(t, path); got != fixtureMirror {
		t.Errorf("%s = %q, want exactly %q with no trailing slash or newline", path, got, fixtureMirror)
	}

	// A source line written with a trailing slash must be normalised the same
	// way, and must still compose URLs the mirror serves.
	originalSkip := SkipDownloadSameSize
	SkipDownloadSameSize = true // revalidate the seeded cache instead of refetching it
	defer func() { SkipDownloadSameSize = originalSkip }()

	config := newRepo(t)
	config.Server.Source = []string{fixtureMirror + "/ " + fixtureDist + " " + fixtureComponent}

	if err := downloadIndex(config); err != nil {
		t.Fatalf("downloadIndex with a trailing slash in the source: %v", err)
	}

	path = filepath.Join(config.Destination.Path, DistsCacheName, fixtureDist, DistsCacheNameUrlBase)
	if got := readFile(t, path); got != fixtureMirror {
		t.Errorf("%s = %q, want exactly %q", path, got, fixtureMirror)
	}

	// readURLBase is the reader readDebPackages uses; it must agree.
	base, err := readURLBase(cachedIndexPath(config))
	if err != nil {
		t.Fatalf("readURLBase %s: %v", cachedIndexPath(config), err)
	}
	if base != fixtureMirror {
		t.Errorf("readURLBase(%s) = %q, want %q", cachedIndexPath(config), base, fixtureMirror)
	}
}

// The whole point of downloading the index is what parsePackages can make of
// it, so check the fetched bytes against the shape of a real Debian index.
func TestIntegrationCachedIndexParsesRealPackages(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	indexPath := cachedIndexPath(config)

	packages, err := readDebPackages(indexPath)
	if err != nil {
		t.Fatalf("readDebPackages %s: %v", indexPath, err)
	}

	// bookworm/contrib holds a few hundred packages; anything near zero means
	// an error page or a truncated file was parsed.
	if len(packages) < 100 {
		t.Fatalf("parsed %d packages from %s, want well over 100 for %s/%s",
			len(packages), indexPath, fixtureDist, fixtureComponent)
	}

	// Cross-check the parser against the raw file: every stanza must have
	// produced exactly one package.
	stanzas := 0
	for _, line := range strings.Split(readFile(t, indexPath), "\n") {
		if strings.HasPrefix(line, "Package: ") {
			stanzas++
		}
	}
	if stanzas != len(packages) {
		t.Errorf("%s has %d \"Package:\" lines but parsed into %d packages", indexPath, stanzas, len(packages))
	}

	failures := 0
	report := func(format string, args ...any) {
		failures++
		if failures <= 5 {
			t.Errorf(format, args...)
		}
	}

	seedFound := false

	for _, pkg := range packages {
		if pkg.Package == "" {
			report("%s: a package was parsed with an empty name", indexPath)
			continue
		}
		if pkg.Package == fixtureSeed {
			seedFound = true
		}
		if pkg.Version == "" {
			report("%s: empty Version", pkg.Package)
		}
		// Every binary package in the archive publishes a SHA256, and
		// downloadPool has nothing to verify against without one.
		if len(pkg.SHA256) != 64 {
			report("%s: SHA256 = %q, want 64 hex characters", pkg.Package, pkg.SHA256)
		} else if _, err := hex.DecodeString(pkg.SHA256); err != nil {
			report("%s: SHA256 = %q is not hexadecimal", pkg.Package, pkg.SHA256)
		}
		if size, err := strconv.ParseInt(pkg.Size, 10, 64); err != nil || size <= 0 {
			report("%s: Size = %q, want a positive integer", pkg.Package, pkg.Size)
		}
		if pkg.IndexArch != fixtureArch {
			report("%s: IndexArch = %q, want %q from the binary-%s directory", pkg.Package, pkg.IndexArch, fixtureArch, fixtureArch)
		}
		// A binary-amd64 index contains amd64 packages plus Architecture: all.
		if pkg.Arch != fixtureArch && pkg.Arch != "all" {
			report("%s: Architecture = %q, want %q or \"all\" in a binary-%s index", pkg.Package, pkg.Arch, fixtureArch, fixtureArch)
		}
		// The pool is laid out per component, and downloadPool joins this onto
		// the mirror URL verbatim.
		if want := "pool/" + fixtureComponent + "/"; !strings.HasPrefix(pkg.Filename, want) {
			report("%s: Filename = %q, want it to start with %q", pkg.Package, pkg.Filename, want)
		}
		if pkg.FilenameUrl != fixtureMirror {
			report("%s: FilenameUrl = %q, want %q from url_base.txt", pkg.Package, pkg.FilenameUrl, fixtureMirror)
		}
	}

	if failures > 5 {
		t.Errorf("%d further field problems were suppressed", failures-5)
	}

	if !seedFound {
		t.Errorf("the fixture seed %q is no longer in %s/%s/%s; the integration fixture needs to be rechosen",
			fixtureSeed, fixtureDist, fixtureComponent, fixtureArch)
	}
}

// A second -di must revalidate the cache cheaply instead of refetching it, and
// must not damage what the first one produced.
func TestIntegrationDownloadIndexIsIdempotent(t *testing.T) {
	requireNetwork(t)

	originalSkip := SkipDownloadSameSize
	SkipDownloadSameSize = true
	defer func() { SkipDownloadSameSize = originalSkip }()

	config := newRepo(t)
	plain := cachedIndexPath(config)
	compressed := filepath.Join(filepath.Dir(plain), PackagesExtensionPreference[0])

	// runIndex runs downloadIndex and returns the compressed index's checksum
	// and mtime, so the caller can tell "skipped" from "fetched again".
	runIndex := func(label string) (string, os.FileInfo) {
		t.Helper()

		if err := downloadIndex(config); err != nil {
			t.Fatalf("%s downloadIndex: %v", label, err)
		}
		_, sha, _, err := hashFile(compressed)
		if err != nil {
			t.Fatalf("hashFile %s: %v", compressed, err)
		}
		info, err := os.Stat(compressed)
		if err != nil {
			t.Fatalf("stat %s: %v", compressed, err)
		}
		return sha, info
	}

	_, seedSHA, _, err := hashFile(compressed)
	if err != nil {
		t.Fatalf("hashFile %s: %v", compressed, err)
	}
	seedInfo, err := os.Stat(compressed)
	if err != nil {
		t.Fatalf("stat %s: %v", compressed, err)
	}

	firstSHA, firstInfo := runIndex("first")
	if firstSHA != seedSHA {
		// A point release landed between seeding the cache and this run, so the
		// refetch was correct and nothing below is comparable.
		t.Skipf("the upstream index changed while the test was running (%s -> %s)", seedSHA, firstSHA)
	}
	// The content is unchanged, so the size heuristic should have skipped the
	// transfer and left the file alone.
	if !firstInfo.ModTime().Equal(seedInfo.ModTime()) {
		t.Errorf("%s was rewritten (mtime %s -> %s) although its content and size still match the mirror: skipDownloadSameSize did not skip it",
			compressed, seedInfo.ModTime(), firstInfo.ModTime())
	}

	_, firstPlainSHA, _, err := hashFile(plain)
	if err != nil {
		t.Fatalf("hashFile %s: %v", plain, err)
	}
	firstPackages, err := readDebPackages(plain)
	if err != nil {
		t.Fatalf("readDebPackages %s: %v", plain, err)
	}

	secondSHA, secondInfo := runIndex("second")
	if secondSHA != firstSHA {
		t.Skipf("the upstream index changed between the two runs (%s -> %s)", firstSHA, secondSHA)
	}
	if !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Errorf("%s was downloaded again by the second run (mtime %s -> %s)",
			compressed, firstInfo.ModTime(), secondInfo.ModTime())
	}

	// The decompressed index is rewritten on every run, so it must at least be
	// reproduced byte for byte and still parse into the same packages.
	_, secondPlainSHA, _, err := hashFile(plain)
	if err != nil {
		t.Fatalf("hashFile %s: %v", plain, err)
	}
	if secondPlainSHA != firstPlainSHA {
		t.Errorf("%s changed across two identical runs (sha256 %s -> %s)", plain, firstPlainSHA, secondPlainSHA)
	}

	secondPackages, err := readDebPackages(plain)
	if err != nil {
		t.Fatalf("readDebPackages %s after the second run: %v", plain, err)
	}
	if len(firstPackages) == 0 {
		t.Fatalf("%s parsed into no packages at all", plain)
	}
	if len(secondPackages) != len(firstPackages) {
		t.Errorf("%s parsed into %d packages after the first run and %d after the second",
			plain, len(firstPackages), len(secondPackages))
	}
}

// A typo in a source line must be reported, must not stop the other sources,
// and above all must not leave a file that -ci would later read as an index.
func TestIntegrationDownloadIndexReportsMissingDist(t *testing.T) {
	requireNetwork(t)

	const missingDist = "no-such-dist-tinyrepo-integration"

	originalSkip := SkipDownloadSameSize
	SkipDownloadSameSize = true // revalidate the healthy target instead of refetching it
	defer func() { SkipDownloadSameSize = originalSkip }()

	config := newRepo(t)
	config.Server.Source = []string{
		fixtureSource(),
		fixtureMirror + " " + missingDist + " " + fixtureComponent,
	}

	err := downloadIndex(config)
	if err == nil {
		t.Fatalf("downloadIndex succeeded although %q does not exist on %s", missingDist, fixtureMirror)
	}
	// One of the two targets failed, and downloadIndex reports how many.
	if !strings.Contains(err.Error(), "1/2") {
		t.Errorf("error = %v, want it to report 1 failed target out of 2", err)
	}

	// The healthy source in the same run is unaffected.
	packages, err := readDebPackages(cachedIndexPath(config))
	if err != nil {
		t.Fatalf("readDebPackages %s after a partial failure: %v", cachedIndexPath(config), err)
	}
	if len(packages) < 100 {
		t.Errorf("%s parsed into %d packages after a partial failure, want well over 100",
			cachedIndexPath(config), len(packages))
	}

	// createDists picks up any file named "Packages", so a 404 body saved under
	// that name would silently poison the generated repository.
	missingDir := filepath.Join(config.Destination.Path, DistsCacheName, missingDist)
	if fileExists(missingDir) {
		for _, name := range PackagesExtensionPreference {
			found, findErr := recursiveFinder(missingDir, name)
			if findErr != nil {
				t.Fatalf("scan %s: %v", missingDir, findErr)
			}
			if len(found) > 0 {
				t.Errorf("the failed download left %s behind: %v", name, found)
			}
		}
	}
}

// parseSources composes the URLs every download depends on. The expansion
// itself is unit tested; what needs the network is whether the composed URLs
// are ones the mirror actually serves.
func TestIntegrationSourceDirURLsResolveOnTheMirror(t *testing.T) {
	requireNetwork(t)

	source := fixtureMirror + " " + fixtureDist + " main " + fixtureComponent
	targets := parseSources([]string{source}, []string{fixtureArch})

	// Two components, one architecture. Without this the loop below could pass
	// while asserting nothing.
	if len(targets) != 2 {
		t.Fatalf("parseSources(%q) returned %d targets, want 2 (one per component)", source, len(targets))
	}

	for _, target := range targets {
		url := target.sourceDir() + "/" + PackagesExtensionPreference[0]
		if status := indexHeadStatus(t, url); status != http.StatusOK {
			t.Errorf("HEAD %s returned %d, want 200: sourceDir() does not compose a URL the mirror serves", url, status)
		}
	}
}
