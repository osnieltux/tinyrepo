package tinyrepo

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// These tests download a real slice of the Debian archive into a stable
// directory under /tmp and run the production code paths against it.
//
// They are skipped by "go test -short", and they skip - never fail - when the
// mirror cannot be reached, so an offline checkout still runs green.
//
// The slice is deliberately tiny: bookworm/contrib/binary-amd64 is about 53 KB
// compressed and holds ~300 real packages, and the seed package pulls in a
// handful of ~3 KB .deb files.
const (
	fixtureMirror    = "http://deb.debian.org/debian"
	fixtureDist      = "bookworm"
	fixtureComponent = "contrib"
	fixtureArch      = "amd64"

	// astrometry-data-2mass is Architecture: all and depends on nine sibling
	// packages that all live inside contrib, while those siblings depend on
	// astrometry.net and curl which live in main. One seed therefore exercises
	// both a fully resolving fan-out and genuine unresolved reporting.
	fixtureSeed = "astrometry-data-2mass"
)

func fixtureSource() string {
	return fixtureMirror + " " + fixtureDist + " " + fixtureComponent
}

// integrationRoot is stable rather than t.TempDir() so the downloaded slice
// survives between runs and the mirror is hit as little as possible.
func integrationRoot() string {
	if dir := os.Getenv("TINYREPO_TEST_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "tinyrepo-integration")
}

var (
	networkOnce sync.Once
	networkErr  error
)

// requireNetwork skips the calling test unless the fixture mirror is actually
// reachable.
func requireNetwork(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	networkOnce.Do(func() {
		config := defaultConfig()
		if networkErr = initHTTPClient(&config); networkErr != nil {
			return
		}

		probe := &http.Client{Transport: httpClient.Transport, Timeout: 20 * time.Second}
		url := fixtureMirror + "/dists/" + fixtureDist + "/Release"

		resp, err := probe.Head(url)
		if err != nil {
			networkErr = err
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			networkErr = fmt.Errorf("HEAD %s: %s", url, resp.Status)
		}
	})

	if networkErr != nil {
		t.Skipf("%s is unreachable: %v", fixtureMirror, networkErr)
	}
}

var (
	sharedCacheOnce sync.Once
	sharedCachePath string
	sharedCacheErr  error
)

// sharedCache downloads the fixture index once per test binary run, caching it
// on disk across runs, and returns the dists_cache directory holding it.
//
// Tests must not mutate what this returns; use newRepo for a writable copy.
func sharedCache(t *testing.T) string {
	t.Helper()
	requireNetwork(t)

	sharedCacheOnce.Do(func() {
		root := filepath.Join(integrationRoot(), "cache")

		config := defaultConfig()
		config.Destination.Path = root
		config.Destination.Arch = []string{fixtureArch}
		config.Destination.Packages = []string{fixtureSeed}
		config.Server.Source = []string{fixtureSource()}

		// Revalidate the on-disk cache cheaply instead of refetching it, and put
		// the global back: this runs inside whichever test happens to call
		// newRepo first, and leaving it changed would make every later test
		// depend on the run order.
		originalSkip := SkipDownloadSameSize
		SkipDownloadSameSize = true
		defer func() { SkipDownloadSameSize = originalSkip }()

		if sharedCacheErr = initHTTPClient(&config); sharedCacheErr != nil {
			return
		}
		if sharedCacheErr = downloadIndex(&config); sharedCacheErr != nil {
			return
		}
		sharedCachePath = filepath.Join(root, DistsCacheName)
	})

	if sharedCacheErr != nil {
		t.Fatalf("could not fetch the fixture index: %v", sharedCacheErr)
	}
	return sharedCachePath
}

// newRepo returns a config pointing at a fresh per-test destination directory,
// pre-seeded with a copy of the shared index cache so the test may mutate it.
func newRepo(t *testing.T, packages ...string) *Config {
	t.Helper()

	cache := sharedCache(t)

	if len(packages) == 0 {
		packages = []string{fixtureSeed}
	}

	dest := filepath.Join(integrationRoot(), "runs", t.Name())
	if err := os.RemoveAll(dest); err != nil {
		t.Fatalf("clean %s: %v", dest, err)
	}
	if err := copyTree(cache, filepath.Join(dest, DistsCacheName)); err != nil {
		t.Fatalf("seed the index cache: %v", err)
	}

	config := defaultConfig()
	config.Destination.Path = dest
	config.Destination.Arch = []string{fixtureArch}
	config.Destination.Packages = packages
	config.Server.Source = []string{fixtureSource()}
	return &config
}

// cachedIndexPath is the decompressed fixture index inside a config's cache.
func cachedIndexPath(config *Config) string {
	return filepath.Join(config.Destination.Path, DistsCacheName,
		fixtureDist, fixtureComponent, "binary-"+fixtureArch, "Packages")
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}
