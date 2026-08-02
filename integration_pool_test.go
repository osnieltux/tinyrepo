package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Real .deb downloads and checksum enforcement.
// See integration_test.go for the shared fixture and scaffolding.

// debfetchBuildRepo runs the whole -ci pipeline against the seeded index cache
// and returns the selected packages together with the manifest read back from
// disk.
func debfetchBuildRepo(t *testing.T, config *Config) (map[string][]*PackageDeb, []manifestEntry) {
	t.Helper()

	selected, _, err := createDists(config)
	if err != nil {
		t.Fatalf("createDists(%s): %v", config.Destination.Path, err)
	}

	dists, err := generateDists(config, selected)
	if err != nil {
		t.Fatalf("generateDists(%s): %v", config.Destination.Path, err)
	}
	if err := generateRelease(dists); err != nil {
		t.Fatalf("generateRelease(%s): %v", dists.Root, err)
	}
	if err := writeManifest(config, selected); err != nil {
		t.Fatalf("writeManifest(%s): %v", manifestPath(config), err)
	}

	entries, err := readManifest(manifestPath(config))
	if err != nil {
		t.Fatalf("readManifest(%s): %v", manifestPath(config), err)
	}
	if len(entries) == 0 {
		t.Fatalf("%s: no entries, the seed %q resolved to nothing", manifestPath(config), fixtureSeed)
	}
	return selected, entries
}

// debfetchPoolPath is where downloadPool stores the .deb of a manifest entry.
func debfetchPoolPath(config *Config, entry manifestEntry) string {
	return filepath.Join(config.Destination.Path, filepath.FromSlash(entry.PoolPath))
}

// debfetchManifestLines splits the manifest into lines and reports, for each
// one, whether it is an entry rather than a comment or the trailing blank.
func debfetchManifestLines(t *testing.T, config *Config) (lines []string, isEntry []bool) {
	t.Helper()

	lines = strings.Split(readFile(t, manifestPath(config)), "\n")
	isEntry = make([]bool, len(lines))

	for i, line := range lines {
		isEntry[i] = !strings.HasPrefix(line, "#") && len(strings.Split(line, "\t")) == 4
	}
	return lines, isEntry
}

// debfetchKeepOneEntry trims the manifest down to a single real entry, so a
// test costs one ~3 KB request instead of the whole fan-out.
func debfetchKeepOneEntry(t *testing.T, config *Config, entries []manifestEntry) manifestEntry {
	t.Helper()

	if len(entries) == 0 {
		t.Fatalf("%s: the manifest is empty", manifestPath(config))
	}
	wanted := entries[0].PoolPath
	path := manifestPath(config)

	lines, isEntry := debfetchManifestLines(t, config)
	var kept []string

	for i, line := range lines {
		if isEntry[i] && strings.Split(line, "\t")[1] != wanted {
			continue
		}
		kept = append(kept, line)
	}

	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0644); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}

	trimmed, err := readManifest(path)
	if err != nil {
		t.Fatalf("readManifest(%s) after trimming: %v", path, err)
	}
	if len(trimmed) != 1 {
		t.Fatalf("%s: %d entries after trimming to %s, want 1", path, len(trimmed), wanted)
	}
	return trimmed[0]
}

// debfetchEditManifest edits in place the tab separated fields of the single
// manifest line describing poolPath.
func debfetchEditManifest(t *testing.T, config *Config, poolPath string, edit func(fields []string)) {
	t.Helper()

	path := manifestPath(config)
	lines, isEntry := debfetchManifestLines(t, config)
	edited := 0

	for i, line := range lines {
		fields := strings.Split(line, "\t")
		if !isEntry[i] || fields[1] != poolPath {
			continue
		}
		edit(fields)
		lines[i] = strings.Join(fields, "\t")
		edited++
	}

	if edited != 1 {
		t.Fatalf("%s: edited %d lines for pool path %s, want exactly 1", path, edited, poolPath)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
}

// debfetchAssertNoResidue fails when a rejected download left anything behind,
// including a half written file or a .tinyrepo.tmp temporary.
func debfetchAssertNoResidue(t *testing.T, dir string) {
	t.Helper()

	found, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return // nothing was created at all, which is what we want
		}
		t.Fatalf("read %s: %v", dir, err)
	}

	for _, item := range found {
		info, statErr := item.Info()
		size := int64(-1)
		if statErr == nil {
			size = info.Size()
		}
		t.Errorf("%s: a rejected download left %s behind (%d bytes)", dir, item.Name(), size)
	}
}

// debfetchVerifyDeb checks that the file on disk is exactly what the manifest
// promised, and that it really is a Debian package.
func debfetchVerifyDeb(t *testing.T, path string, entry manifestEntry) {
	t.Helper()

	_, sha256Hex, size, err := hashFile(path)
	if err != nil {
		t.Errorf("%s (manifest entry %s): %v", path, entry.PoolPath, err)
		return
	}
	if size != entry.Size {
		t.Errorf("%s: %d bytes on disk, the manifest says %d", path, size, entry.Size)
	}
	if !strings.EqualFold(sha256Hex, entry.SHA256) {
		t.Errorf("%s: sha256 on disk is %s, the manifest says %s", path, sha256Hex, entry.SHA256)
	}

	// A .deb is an ar archive: without this an error page of the right length
	// would pass unnoticed.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	if magic := string(data[:min(len(data), 8)]); magic != "!<arch>\n" {
		t.Errorf("%s: starts with %q, want the ar magic %q", path, magic, "!<arch>\n")
	}
}

// The full -ci then -dp pipeline against the real archive: every manifest entry
// must end up on disk with the size and checksum the mirror published.
func TestIntegrationDownloadPoolFetchesRealDebs(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	config.Settings.VerifyChecksum = true

	_, entries := debfetchBuildRepo(t, config)

	// The seed depends on siblings that live in the same component, so a real
	// fan-out must produce several pool files.
	if len(entries) < 2 {
		t.Fatalf("%s: %d manifest entries for seed %q, want at least 2 (the seed plus its siblings)",
			manifestPath(config), len(entries), fixtureSeed)
	}

	if err := downloadPool(config); err != nil {
		t.Fatalf("downloadPool(%s): %v", config.Destination.Path, err)
	}

	for _, entry := range entries {
		debfetchVerifyDeb(t, debfetchPoolPath(config, entry), entry)
	}
}

// The manifest written from a real index must be usable: the mirror it came
// from, a pool path, a positive size and a full SHA256 for every package the
// resolver selected.
func TestIntegrationManifestRoundTripOnRealIndex(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	selected, entries := debfetchBuildRepo(t, config)

	byPool := make(map[string]manifestEntry, len(entries))

	for _, entry := range entries {
		if _, duplicate := byPool[entry.PoolPath]; duplicate {
			t.Errorf("%s appears twice in the manifest, one pool file must have one entry", entry.PoolPath)
		}
		byPool[entry.PoolPath] = entry

		if entry.BaseURL != fixtureMirror {
			t.Errorf("%s: BaseURL = %q, want the configured mirror %q", entry.PoolPath, entry.BaseURL, fixtureMirror)
		}
		if !strings.HasPrefix(entry.PoolPath, "pool/") {
			t.Errorf("PoolPath = %q, want it to start with %q", entry.PoolPath, "pool/")
		}
		if !strings.HasSuffix(entry.PoolPath, ".deb") {
			t.Errorf("PoolPath = %q, want it to end with %q", entry.PoolPath, ".deb")
		}
		if entry.Size <= 0 {
			t.Errorf("%s: Size = %d, want a positive size", entry.PoolPath, entry.Size)
		}
		if len(entry.SHA256) != 64 {
			t.Errorf("%s: SHA256 = %q is %d characters, want 64", entry.PoolPath, entry.SHA256, len(entry.SHA256))
		} else if _, err := hex.DecodeString(entry.SHA256); err != nil {
			t.Errorf("%s: SHA256 = %q is not hexadecimal: %v", entry.PoolPath, entry.SHA256, err)
		}
	}

	// Every package the resolver picked must be downloadable, with the size and
	// checksum the index published for it.
	seedSelected := false
	checked := 0

	for arch, packages := range selected {
		for _, pkg := range packages {
			if pkg.Package == fixtureSeed {
				seedSelected = true
			}
			if pkg.Filename == "" {
				t.Errorf("%s (%s): the index stanza has no Filename", pkg.Package, arch)
				continue
			}

			entry, ok := byPool[pkg.Filename]
			if !ok {
				t.Errorf("%s (%s) was selected but %s is missing from the manifest", pkg.Package, arch, pkg.Filename)
				continue
			}
			if got := strconv.FormatInt(entry.Size, 10); got != pkg.Size {
				t.Errorf("%s: manifest size %s, index says %s", pkg.Filename, got, pkg.Size)
			}
			if !strings.EqualFold(entry.SHA256, pkg.SHA256) {
				t.Errorf("%s: manifest sha256 %s, index says %s", pkg.Filename, entry.SHA256, pkg.SHA256)
			}
			checked++
		}
	}

	if !seedSelected {
		t.Errorf("the seed %q is missing from the packages selected out of %s", fixtureSeed, cachedIndexPath(config))
	}
	if checked != len(entries) {
		t.Errorf("%d selected packages cross-checked against %d manifest entries, want the same number",
			checked, len(entries))
	}
}

// Overwriting a downloaded .deb with same sized garbage must be repaired on the
// next run: proof that the checksum is recomputed instead of trusting the file
// that is already there.
func TestIntegrationDownloadPoolRepairsCorruptedDeb(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	config.Settings.VerifyChecksum = true

	_, entries := debfetchBuildRepo(t, config)
	entry := debfetchKeepOneEntry(t, config, entries)
	path := debfetchPoolPath(config, entry)

	if err := downloadPool(config); err != nil {
		t.Fatalf("downloadPool(%s): %v", config.Destination.Path, err)
	}
	debfetchVerifyDeb(t, path, entry)

	// Same length, different content: only a checksum can tell the difference.
	garbage := []byte(strings.Repeat("x", int(entry.Size)))
	if err := os.WriteFile(path, garbage, 0644); err != nil {
		t.Fatalf("corrupt %s: %v", path, err)
	}

	if err := downloadPool(config); err != nil {
		t.Fatalf("downloadPool did not repair the corrupted %s: %v", path, err)
	}
	debfetchVerifyDeb(t, path, entry)
}

// Without checksum verification the size heuristic must still notice a
// truncated .deb and fetch it again.
func TestIntegrationDownloadPoolRestoresTruncatedDeb(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	config.Settings.VerifyChecksum = false

	// Set after newRepo: sharedCache assigns this global on its first call.
	originalSkip := SkipDownloadSameSize
	SkipDownloadSameSize = true
	defer func() { SkipDownloadSameSize = originalSkip }()

	_, entries := debfetchBuildRepo(t, config)
	entry := debfetchKeepOneEntry(t, config, entries)
	path := debfetchPoolPath(config, entry)

	if err := downloadPool(config); err != nil {
		t.Fatalf("downloadPool(%s): %v", config.Destination.Path, err)
	}
	debfetchVerifyDeb(t, path, entry)

	truncated := entry.Size / 2
	if truncated == 0 {
		t.Fatalf("%s: the manifest size is %d, too small to truncate", entry.PoolPath, entry.Size)
	}
	if err := os.Truncate(path, truncated); err != nil {
		t.Fatalf("truncate %s to %d bytes: %v", path, truncated, err)
	}

	if err := downloadPool(config); err != nil {
		t.Fatalf("downloadPool did not restore the truncated %s: %v", path, err)
	}
	debfetchVerifyDeb(t, path, entry)
}

// A manifest whose SHA256 does not describe the real file must fail loudly and
// leave nothing behind.
func TestIntegrationDownloadPoolRejectsTamperedManifestChecksum(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	config.Settings.VerifyChecksum = true

	_, entries := debfetchBuildRepo(t, config)
	entry := debfetchKeepOneEntry(t, config, entries)
	path := debfetchPoolPath(config, entry)

	// Wrong, but indistinguishable from a real checksum by shape alone.
	tampered := strings.Repeat("0123456789abcdef", 4)
	if strings.EqualFold(tampered, entry.SHA256) {
		t.Fatalf("%s: the tampered checksum equals the real one", entry.PoolPath)
	}
	debfetchEditManifest(t, config, entry.PoolPath, func(fields []string) { fields[3] = tampered })

	if err := downloadPool(config); err == nil {
		t.Fatalf("downloadPool accepted %s whose manifest checksum is %s instead of %s",
			entry.PoolPath, tampered, entry.SHA256)
	}
	if fileExists(path) {
		t.Errorf("%s was kept even though it does not match the manifest checksum", path)
	}
	debfetchAssertNoResidue(t, filepath.Dir(path))
}

// With verifyChecksum disabled the size recorded in the manifest is the only
// guard left, and it must still reject a file of the wrong length.
func TestIntegrationDownloadPoolRejectsWrongManifestSize(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	config.Settings.VerifyChecksum = false

	// Set after newRepo, which is what triggers sharedCache and its assignment
	// to this global. Off, so the HEAD heuristic cannot decide the outcome.
	originalSkip := SkipDownloadSameSize
	SkipDownloadSameSize = false
	defer func() { SkipDownloadSameSize = originalSkip }()

	_, entries := debfetchBuildRepo(t, config)
	entry := debfetchKeepOneEntry(t, config, entries)
	path := debfetchPoolPath(config, entry)

	wrongSize := entry.Size + 4096
	debfetchEditManifest(t, config, entry.PoolPath, func(fields []string) {
		fields[2] = strconv.FormatInt(wrongSize, 10)
	})

	if err := downloadPool(config); err == nil {
		t.Fatalf("downloadPool accepted %s: the manifest claims %d bytes, the real file is %d",
			entry.PoolPath, wrongSize, entry.Size)
	}
	if fileExists(path) {
		t.Errorf("%s was kept even though its size does not match the manifest", path)
	}
	debfetchAssertNoResidue(t, filepath.Dir(path))
}
