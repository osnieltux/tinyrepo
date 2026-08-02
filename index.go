package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// indexTarget is one "binary-<arch>" index to fetch, expanded from a single
// [server].source line.
type indexTarget struct {
	BaseURL   string // http://deb.debian.org/debian
	Dist      string // bookworm
	Component string // main
	Arch      string // amd64
}

// sourceDir is the remote directory holding the Packages files.
func (t indexTarget) sourceDir() string {
	return fmt.Sprintf("%s/dists/%s/%s/binary-%s",
		strings.TrimRight(t.BaseURL, "/"), t.Dist, t.Component, t.Arch)
}

// cacheDir is the local directory the index is cached in.
func (t indexTarget) cacheDir(destinationPath string) string {
	return filepath.Join(destinationPath, DistsCacheName, t.Dist, t.Component, "binary-"+t.Arch)
}

// urlBasePath is where the mirror URL of this dist is recorded, so that -ci can
// later tell which mirror each cached package came from.
func (t indexTarget) urlBasePath(destinationPath string) string {
	return filepath.Join(destinationPath, DistsCacheName, t.Dist, DistsCacheNameUrlBase)
}

// parseSources expands each source line into one target per component/arch.
// A line looks like: "http://deb.debian.org/debian bookworm main contrib".
func parseSources(sources []string, architectures []string) []indexTarget {
	var targets []indexTarget

	for _, src := range sources {
		parts := strings.Fields(src)
		if len(parts) < 3 {
			logError(_t("config error"), ": [server].source:", src)
			continue
		}

		baseURL, dist, components := parts[0], parts[1], parts[2:]
		for _, component := range components {
			for _, arch := range architectures {
				targets = append(targets, indexTarget{
					BaseURL:   baseURL,
					Dist:      dist,
					Component: component,
					Arch:      arch,
				})
			}
		}
	}
	return targets
}

// downloadIndex fetches the upstream Packages index for every target into the
// local cache and decompresses it.
func downloadIndex(config *Config) error {
	targets := parseSources(config.Server.Source, config.Destination.Arch)
	if len(targets) == 0 {
		return fmt.Errorf("[server].source: %s", _t("it i m a was n sp"))
	}

	if err := writeURLBaseFiles(config.Destination.Path, targets); err != nil {
		return err
	}

	// One Release per suite, read before the concurrent phase so every target
	// of that suite can verify against it without refetching or locking.
	releases := fetchReleaseIndexes(targets)

	var failed atomic.Int64
	tasks := make([]func(), 0, len(targets))

	for _, target := range targets {
		tasks = append(tasks, func() {
			if err := fetchTargetIndex(config.Destination.Path, target, releases[target.Dist]); err != nil {
				logErrorf("%s %s: %v", _t("error d"), target.sourceDir(), err)
				failed.Add(1)
			}
		})
	}

	runConcurrent(config.Settings.MaxConcurrentDownloads, tasks)

	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%s %d/%d", _t("d failed"), n, len(targets))
	}
	return nil
}

// fetchTargetIndex tries each compression variant in order of preference and
// stops at the first one the mirror actually serves.
//
// release is the suite's upstream Release, or nil when it could not be read. It
// carries the SHA256 of each variant and of the decompressed Packages, so it
// both verifies the download and gives decompression an exact ceiling.
func fetchTargetIndex(destinationPath string, target indexTarget, release *releaseIndex) error {
	cacheDir := target.cacheDir(destinationPath)
	var lastErr error

	// What the plain Packages must expand to. Zero when Release did not say,
	// in which case decompression falls back to the general ceiling.
	plain, verifiable := release.check(target.Component, target.Arch, "Packages")

	for _, extension := range PackagesExtensionPreference {
		sourceURL := target.sourceDir() + "/" + extension
		dest := filepath.Join(cacheDir, extension)

		want, listed := release.check(target.Component, target.Arch, extension)
		if !listed {
			// Either the mirror has no Release, or it does not mention this
			// variant. Say so rather than let it pass quietly: everything built
			// afterwards inherits whatever this file contains.
			logErrorf("%s: %s", _t("index unverified"), sourceURL)
		}

		logDebug(_t("starting d"), sourceURL)

		if err := downloadFile(sourceURL, dest, want); err != nil {
			logDebugf("%s %s: %v", _t("error d"), sourceURL, err)
			lastErr = err
			continue
		}

		if extension == "Packages" {
			return nil // already plain text
		}

		limit := int64(MaxIndexSize)
		if verifiable && plain.Size > 0 {
			limit = plain.Size
		}

		// Past this point a variant can still turn out to be unusable, and each
		// remaining one is a whole file of its own: a mirror with a broken .xz
		// usually publishes a perfectly good .gz beside it. So a bad variant is
		// treated like a failed download - remembered, and stepped over.
		if err := decompressLimit(dest, limit); err != nil {
			lastErr = fmt.Errorf("%s %s: %v", _t("err decompress"), dest, err)
			logErrorf("%s %s: %v", _t("err decompress"), sourceURL, err)
			logError(_t("trying next"), sourceURL)
			continue
		}

		// The compressed file matching proves the transfer, but the expansion
		// is what everything downstream actually reads.
		if verifiable {
			expanded := filepath.Join(cacheDir, "Packages")
			if !plain.matchesLocal(expanded) {
				// The wrong expansion is already in place, and -ci reads that
				// file without ever rechecking it. Removing it means a run where
				// every variant fails leaves no index at all, which -ci reports,
				// rather than one that looks fine and is not.
				os.Remove(expanded)

				lastErr = fmt.Errorf("%s: %s", _t("checksum mismatch"), expanded)
				logErrorf("%s: %s", _t("checksum mismatch"), sourceURL)
				logError(_t("trying next"), sourceURL)
				continue
			}
			logDebugf("%s %s", _t("index verified"), sourceURL)
		}
		return nil
	}

	return fmt.Errorf("%s: %v", _t("error d"), lastErr)
}

// writeURLBaseFiles records, per dist, which mirror it came from.
func writeURLBaseFiles(destinationPath string, targets []indexTarget) error {
	seen := map[string]string{} // dist -> base URL

	for _, target := range targets {
		path := target.urlBasePath(destinationPath)

		if previous, ok := seen[path]; ok {
			if previous != target.BaseURL {
				logError(_t("dup dist"), target.Dist, previous, target.BaseURL)
			}
			continue
		}
		seen[path] = target.BaseURL

		baseURL := strings.TrimRight(target.BaseURL, "/")
		err := writeFileAtomic(path, func(w io.Writer) error {
			_, err := io.WriteString(w, baseURL)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s %s: %v", _t("err g url_b.txt"), path, err)
		}
	}
	return nil
}
