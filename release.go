package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// releaseIndex is what an upstream Release publishes about the files under
// dists/<suite>/: the SHA256 and size of each one.
//
// It is what makes an index download verifiable. Without it the Packages file
// everything else is built from arrives with no integrity check at all, and a
// tampered index poisons every package that follows.
//
// It is worth being clear about the limit: nothing here checks a signature, so
// this catches a corrupt mirror or a mangled file, not an attacker who can
// rewrite Release too. Over plain http that is exactly what a man in the middle
// can do; https, or the GPG verification still on the TODO list, is what closes
// that gap.
type releaseIndex struct {
	Files map[string]fileCheck
}

// releaseURL is where a suite publishes its Release.
func releaseURL(baseURL, dist string) string {
	return strings.TrimRight(baseURL, "/") + "/dists/" + dist + "/Release"
}

// fetchReleaseIndex downloads and parses the Release of one suite.
func fetchReleaseIndex(baseURL, dist string) (*releaseIndex, error) {
	url := releaseURL(baseURL, dist)

	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", _t("error d"), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %s", _t("unexpected status"), resp.Status, url)
	}

	return parseReleaseIndex(io.LimitReader(resp.Body, MaxIndexSize))
}

// parseReleaseIndex reads the SHA256 block of a Release file.
//
// The format is a deb822 stanza where SHA256 is a multi-line field, each
// continuation line being "<hex> <size> <path>". MD5Sum and SHA1 have the same
// shape and are ignored: a weaker digest adds nothing here.
func parseReleaseIndex(r io.Reader) (*releaseIndex, error) {
	index := &releaseIndex{Files: map[string]fileCheck{}}
	inSHA256 := false

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, maxIndexLineSize), maxIndexLineSize)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// A continuation line is indented; anything else starts a new field and
		// therefore ends the block we care about.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inSHA256 = strings.HasPrefix(line, "SHA256:")
			continue
		}
		if !inSHA256 {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}

		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}

		// First entry wins: with Acquire-By-Hash a path can be listed more than
		// once, and the first is the canonical one.
		if _, exists := index.Files[fields[2]]; !exists {
			index.Files[fields[2]] = fileCheck{SHA256: fields[0], Size: size}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(index.Files) == 0 {
		return nil, fmt.Errorf("%s", _t("err release empty"))
	}
	return index, nil
}

// check returns what the named index file has to match, for example
// "main/binary-amd64/Packages.xz". The second result is false when Release does
// not mention it, which is the caller's cue to carry on unverified.
func (r *releaseIndex) check(component, arch, name string) (fileCheck, bool) {
	if r == nil {
		return fileCheck{}, false
	}
	want, ok := r.Files[path.Join(component, "binary-"+arch, name)]
	return want, ok
}

// fetchReleaseIndexes reads the Release of every distinct suite named in the
// sources, once each. A suite whose Release cannot be read maps to nil, and its
// indexes are then downloaded unverified with a warning.
func fetchReleaseIndexes(targets []indexTarget) map[string]*releaseIndex {
	byDist := map[string]*releaseIndex{}

	for _, target := range targets {
		if _, done := byDist[target.Dist]; done {
			continue
		}

		index, err := fetchReleaseIndex(target.BaseURL, target.Dist)
		if err != nil {
			logErrorf("%s %s: %v", _t("err release"), target.Dist, err)
			byDist[target.Dist] = nil
			continue
		}

		logDebugf("%s %s: %d", _t("release read"), target.Dist, len(index.Files))
		byDist[target.Dist] = index
	}
	return byDist
}
