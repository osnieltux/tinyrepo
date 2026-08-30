package tinyrepo

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// cleanRepo deletes every package file in the destination that is not one of
// the packages declared in config.toml, nor one of their dependencies.
//
// It is how packages fetched on demand are removed again: those are never
// written back into config.toml, so the declared closure is the only thing that
// says which of them should stay. Nothing it deletes is irreplaceable - every
// file it removes can be downloaded again by -dp or on demand.
func cleanRepo(config *Config, backend Backend) error {
	cat, err := backend.Catalog(config)
	if err != nil {
		return fmt.Errorf("%s: %v", _t("err catalog"), err)
	}

	keep := map[string]bool{}
	for _, entry := range cat.declared(config.Destination.Packages) {
		keep[entry.PoolPath] = true
	}
	logDebugf("%s %d", _t("keeping"), len(keep))

	// A declared package list that resolves to nothing means the cache is stale
	// or the names are wrong, and deleting the whole repository is not a
	// reasonable reading of that. An empty list, on the other hand, is an
	// unambiguous "keep nothing" - the pure proxy case.
	if len(keep) == 0 && len(config.Destination.Packages) > 0 {
		return fmt.Errorf("%s", _t("err keep set"))
	}

	candidates, err := cleanupCandidates(config.Destination.Path)
	if err != nil {
		return err
	}

	removed, freed := 0, int64(0)
	for _, relative := range cleanupPlan(candidates, keep) {
		full := filepath.Join(config.Destination.Path, filepath.FromSlash(relative))

		size := int64(0)
		if info, err := os.Stat(full); err == nil {
			size = info.Size()
		}
		if err := os.Remove(full); err != nil {
			logErrorf("%s %s: %v", _t("err cleanup"), relative, err)
			continue
		}

		logError(_t("removed"), relative, humanSize(size))
		removed++
		freed += size
	}

	if removed == 0 {
		logError(_t("nothing to remove"))
		return nil
	}

	pruneEmptyDirs(filepath.Join(config.Destination.Path, "pool"))
	logError(_t("freed"), humanSize(freed), fmt.Sprintf("(%d)", removed))
	return nil
}

// protectedNames can never be deleted. The namespace rule below already makes
// them unreachable; listing them anyway keeps that safety property visible in
// the code instead of being an emergent consequence of two other functions.
var protectedNames = map[string]bool{
	"Release":   true,
	"InRelease": true,
	"Packages":  true,
}

// cleanupCandidates lists every file -cl is allowed to consider, as paths
// relative to the destination and in slash form.
//
// Debian packages live under <dest>/pool/; pacman packages are flat files at
// <dest> whose name contains ".pkg.tar". Nothing else is ever a candidate, so
// Release, Packages*, tinyrepo.db*, tinyrepo.files*, dists_cache/, db_cache/
// and .tinyrepo/ cannot be reached from here at all.
func cleanupCandidates(root string) ([]string, error) {
	var candidates []string

	pool := filepath.Join(root, "pool")
	if info, err := os.Stat(pool); err == nil && info.IsDir() {
		err := filepath.WalkDir(pool, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || protectedNames[d.Name()] {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			candidates = append(candidates, filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s %v", _t("err recursive f"), err)
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || protectedNames[name] {
			continue
		}
		// ".pkg.tar" catches nano-8.6-1-x86_64.pkg.tar.zst and its .sig, and
		// misses tinyrepo.db.tar.gz and tinyrepo.files.tar.gz, which is exactly
		// the line that has to be drawn here.
		if strings.Contains(name, ".pkg.tar") || strings.HasSuffix(name, TempSuffix) {
			candidates = append(candidates, name)
		}
	}

	slices.Sort(candidates)
	return candidates, nil
}

// cleanupPlan is the pure half: which candidates go, given what to keep. A
// pacman signature follows its package, and a half-written file always goes.
func cleanupPlan(candidates []string, keep map[string]bool) []string {
	var remove []string

	for _, candidate := range candidates {
		if strings.HasSuffix(candidate, TempSuffix) {
			// Left behind by an interrupted download; never useful.
			remove = append(remove, candidate)
			continue
		}
		if keep[strings.TrimSuffix(candidate, ".sig")] {
			continue
		}
		remove = append(remove, candidate)
	}
	return remove
}

// pruneEmptyDirs removes directories left empty by the deletions, deepest
// first, so that pool/main/n/nano does not survive as an empty shell.
func pruneEmptyDirs(root string) {
	var dirs []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return
	}

	// Deepest first: removing a leaf can make its parent empty in turn.
	slices.Sort(dirs)
	slices.Reverse(dirs)

	for _, dir := range dirs {
		// Fails, harmlessly, when the directory is not empty.
		os.Remove(dir)
	}
}
