package tinyrepo

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Correctness of the generated repository and its Release.
// See integration_test.go for the shared fixture and scaffolding.

// integrationBuildRepo runs the real "-ci" pipeline over the cached fixture
// index and returns what was selected together with what was written.
func integrationBuildRepo(t *testing.T, config *Config) (map[string][]*PackageDeb, *distsResult) {
	t.Helper()

	selected, _, err := createDists(config)
	if err != nil {
		t.Fatalf("createDists(%s): %v", config.Destination.Path, err)
	}

	result, err := generateDists(config, selected)
	if err != nil {
		t.Fatalf("generateDists(%s): %v", config.Destination.Path, err)
	}

	if err := generateRelease(result); err != nil {
		t.Fatalf("generateRelease(%s): %v", result.Root, err)
	}
	return selected, result
}

// integrationReleaseChecksum is one line of a Release MD5Sum/SHA256 block.
type integrationReleaseChecksum struct {
	Hash string
	Size int64
	Path string
}

// integrationParseRelease parses a Release file into its headers and its
// checksum blocks. A header with an empty value ("MD5Sum:") opens a block, and
// every indented line that follows belongs to it.
func integrationParseRelease(t *testing.T, path string) (map[string]string, map[string][]integrationReleaseChecksum) {
	t.Helper()

	headers := map[string]string{}
	blocks := map[string][]integrationReleaseChecksum{}
	block := ""

	for i, line := range strings.Split(readFile(t, path), "\n") {
		lineNo := i + 1

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if block == "" {
				t.Fatalf("%s:%d: indented line outside of a checksum block: %q", path, lineNo, line)
			}

			fields := strings.Fields(line)
			if len(fields) != 3 {
				t.Fatalf("%s:%d: want %q to be \"<hash> <size> <path>\", got %d fields",
					path, lineNo, line, len(fields))
			}

			size, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				t.Fatalf("%s:%d: size %q in %q is not a number: %v", path, lineNo, fields[1], line, err)
			}

			blocks[block] = append(blocks[block], integrationReleaseChecksum{
				Hash: fields[0],
				Size: size,
				Path: fields[2],
			})
			continue
		}

		name, value, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("%s:%d: %q is not a \"Name: value\" header", path, lineNo, line)
		}
		// deb822: the colon is followed by a space, or by nothing at all when
		// the header opens a checksum block.
		if value != "" && !strings.HasPrefix(value, " ") {
			t.Fatalf("%s:%d: %q must separate name and value with \": \"", path, lineNo, line)
		}

		if value = strings.TrimSpace(value); value == "" {
			block = name
			if _, seen := blocks[block]; !seen {
				blocks[block] = nil
			}
			continue
		}

		headers[name] = value
		block = ""
	}

	return headers, blocks
}

// Every checksum published in Release must describe the file that is actually
// on disk: a stale or wrong hash makes apt reject the whole repository.
func TestIntegrationReleaseChecksumsMatchFilesOnDisk(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	_, result := integrationBuildRepo(t, config)

	releasePath := filepath.Join(result.Root, "Release")
	_, blocks := integrationParseRelease(t, releasePath)

	md5Block, sha256Block := blocks["MD5Sum"], blocks["SHA256"]

	if len(md5Block) == 0 || len(sha256Block) == 0 {
		t.Fatalf("%s: got %d MD5Sum and %d SHA256 lines, want one of each per generated file",
			releasePath, len(md5Block), len(sha256Block))
	}
	if len(md5Block) != len(sha256Block) {
		t.Fatalf("%s: the MD5Sum block lists %d files and the SHA256 block lists %d, want the same files in both",
			releasePath, len(md5Block), len(sha256Block))
	}

	for i, sha := range sha256Block {
		md5Line := md5Block[i]

		if md5Line.Path != sha.Path {
			t.Errorf("%s: checksum block entry %d is %q under MD5Sum but %q under SHA256",
				releasePath, i, md5Line.Path, sha.Path)
			continue
		}
		if md5Line.Size != sha.Size {
			t.Errorf("%s: %s has size %d under MD5Sum and %d under SHA256",
				releasePath, sha.Path, md5Line.Size, sha.Size)
		}

		// Paths are relative to dists/<codename>/ and always use forward
		// slashes, whatever the platform the repository was generated on.
		if strings.Contains(sha.Path, `\`) {
			t.Errorf("%s: path %q must use forward slashes", releasePath, sha.Path)
		}
		if strings.HasPrefix(sha.Path, "/") || filepath.IsAbs(sha.Path) {
			t.Errorf("%s: path %q must be relative to %s", releasePath, sha.Path, result.Root)
			continue
		}
		if !strings.HasPrefix(sha.Path, DestinationComponent+"/") {
			t.Errorf("%s: path %q must live under the %q component",
				releasePath, sha.Path, DestinationComponent)
		}

		abs := filepath.Join(result.Root, filepath.FromSlash(sha.Path))

		md5Hex, sha256Hex, size, err := hashFile(abs)
		if err != nil {
			t.Errorf("%s lists %q, but %s cannot be read: %v", releasePath, sha.Path, abs, err)
			continue
		}

		if sha.Size != size {
			t.Errorf("%s: %s is listed with size %d, but the file on disk is %d bytes",
				releasePath, sha.Path, sha.Size, size)
		}
		if !strings.EqualFold(sha.Hash, sha256Hex) {
			t.Errorf("%s: %s is listed with SHA256 %s, but the file on disk hashes to %s",
				releasePath, sha.Path, sha.Hash, sha256Hex)
		}
		if !strings.EqualFold(md5Line.Hash, md5Hex) {
			t.Errorf("%s: %s is listed with MD5 %s, but the file on disk hashes to %s",
				releasePath, sha.Path, md5Line.Hash, md5Hex)
		}
	}
}

// Release must describe the generated tree exactly: an index that is written
// but not listed is invisible to apt, and a listed file that does not exist
// makes apt fail the update.
func TestIntegrationReleaseListsEveryGeneratedFile(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	_, result := integrationBuildRepo(t, config)

	releasePath := filepath.Join(result.Root, "Release")
	_, blocks := integrationParseRelease(t, releasePath)

	listed := map[string]bool{}
	for _, entry := range blocks["SHA256"] {
		if listed[entry.Path] {
			t.Errorf("%s: %s is listed twice in the SHA256 block", releasePath, entry.Path)
		}
		listed[entry.Path] = true
	}

	for _, entry := range result.Entries {
		if !listed[entry.Rel] {
			t.Errorf("generateDists wrote %s but Release does not list %q", entry.Abs, entry.Rel)
		}
	}

	// Nothing under dists/<codename>/ may go unlisted, Release itself aside.
	err := filepath.WalkDir(result.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(result.Root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if rel == "Release" {
			return nil
		}
		if !listed[rel] {
			t.Errorf("%s exists under %s but is not listed in Release", rel, result.Root)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", result.Root, err)
	}

	for path := range listed {
		abs := filepath.Join(result.Root, filepath.FromSlash(path))
		if !fileExists(abs) {
			t.Errorf("%s lists %q, but %s does not exist", releasePath, path, abs)
		}
	}
}

// The Release headers must describe what was really generated, not what the
// upstream slice happened to be built from.
func TestIntegrationReleaseHeadersDescribeTheGeneratedRepository(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	_, result := integrationBuildRepo(t, config)

	releasePath := filepath.Join(result.Root, "Release")
	headers, _ := integrationParseRelease(t, releasePath)

	for _, name := range []string{"Origin", "Label", "Description"} {
		if headers[name] == "" {
			t.Errorf("%s: header %q is missing or empty", releasePath, name)
		}
	}

	if got := headers["Codename"]; got != DestinationDistsName {
		t.Errorf("%s: Codename = %q, want %q", releasePath, got, DestinationDistsName)
	}
	if got := headers["Suite"]; got != DestinationDistsName {
		t.Errorf("%s: Suite = %q, want %q", releasePath, got, DestinationDistsName)
	}

	// The repository is republished under tinyrepo's own component, so the
	// upstream component name must not leak into the header.
	if got := headers["Components"]; got != DestinationComponent {
		t.Errorf("%s: Components = %q, want %q (upstream component was %q)",
			releasePath, got, DestinationComponent, fixtureComponent)
	}

	if got := strings.Fields(headers["Architectures"]); !slices.Equal(got, result.Architectures) {
		t.Errorf("%s: Architectures = %v, want the architectures actually generated %v",
			releasePath, got, result.Architectures)
	}

	// apt parses this header as RFC1123; anything else and the repository is
	// rejected as expired or malformed.
	date, err := time.Parse(time.RFC1123, headers["Date"])
	if err != nil {
		t.Fatalf("%s: Date = %q, which does not parse as RFC1123: %v", releasePath, headers["Date"], err)
	}
	if skew := time.Since(date); skew < -time.Minute || skew > time.Hour {
		t.Errorf("%s: Date = %q, which is %v away from now; it must be stamped when Release is written",
			releasePath, headers["Date"], skew)
	}
}

// apt may fetch any of the three variants, so all three must carry exactly the
// same index.
func TestIntegrationCompressedIndexesMatchPlainPackages(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	_, result := integrationBuildRepo(t, config)

	if len(result.Architectures) == 0 {
		t.Fatalf("generateDists produced no architecture for %s", result.Root)
	}

	for _, arch := range result.Architectures {
		dir := filepath.Join(result.Root, DestinationComponent, "binary-"+arch)
		plainPath := filepath.Join(dir, "Packages")

		want, err := os.ReadFile(plainPath)
		if err != nil {
			t.Fatalf("read %s: %v", plainPath, err)
		}
		if len(want) == 0 {
			t.Fatalf("%s is empty, want the generated index", plainPath)
		}

		for _, extension := range []string{".gz", ".xz"} {
			compressedPath := plainPath + extension

			data, err := os.ReadFile(compressedPath)
			if err != nil {
				t.Errorf("read %s: %v", compressedPath, err)
				continue
			}

			// decompress writes next to its input, so work on a copy to keep
			// the generated index untouched.
			scratch := filepath.Join(t.TempDir(), "Packages"+extension)
			if err := os.WriteFile(scratch, data, 0644); err != nil {
				t.Fatalf("write %s: %v", scratch, err)
			}
			if err := decompress(scratch); err != nil {
				t.Errorf("decompress %s (copy of %s): %v", scratch, compressedPath, err)
				continue
			}

			got, err := os.ReadFile(strings.TrimSuffix(scratch, extension))
			if err != nil {
				t.Errorf("read the output of decompress %s: %v", compressedPath, err)
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s does not decompress to %s: got %d bytes, want the %d bytes of the plain index",
					compressedPath, plainPath, len(got), len(want))
			}
		}
	}
}

// integrationIsFieldName reports whether name is a valid deb822 field name, so
// that any non-standard line is caught and not only the historical
// "#FilenameUrl" one.
func integrationIsFieldName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c == '-' || (c >= '0' && c <= '9')):
		default:
			return false
		}
	}
	return true
}

// The published index must be plain Debian control format: tinyrepo's own
// bookkeeping stays in the manifest, and apt must be able to read back exactly
// what was selected.
func TestIntegrationGeneratedPackagesIsWellFormed(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	selected, result := integrationBuildRepo(t, config)

	if len(result.Architectures) == 0 {
		t.Fatalf("generateDists produced no architecture for %s", result.Root)
	}

	for _, arch := range result.Architectures {
		path := filepath.Join(result.Root, DestinationComponent, "binary-"+arch, "Packages")
		content := readFile(t, path)

		if content == "" {
			t.Fatalf("%s is empty, want the packages selected for %s", path, arch)
		}
		// The seed is expected to pull in its siblings, so a single stanza means
		// the dependency fan-out never reached the generated index.
		if len(selected[arch]) < 2 {
			t.Fatalf("%s: only %d package(s) selected for %s from seed %q, want the seed plus its dependencies",
				path, len(selected[arch]), arch, fixtureSeed)
		}

		if strings.Contains(content, "#FilenameUrl") {
			t.Errorf("%s: the non-standard #FilenameUrl field leaked into the published index", path)
		}
		if !strings.HasSuffix(content, "\n\n") {
			t.Errorf("%s: the last stanza is not terminated by a blank line", path)
		}
		if strings.Contains(content, "\n\n\n") {
			t.Errorf("%s: stanzas are separated by more than one blank line", path)
		}

		for i, line := range strings.Split(content, "\n") {
			lineNo := i + 1

			if line == "" {
				continue
			}
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				t.Errorf("%s:%d: continuation line %q, but every field is written on a single line",
					path, lineNo, line)
				continue
			}

			name, value, ok := strings.Cut(line, ": ")
			if !ok {
				t.Errorf("%s:%d: %q is not a \"Name: value\" field", path, lineNo, line)
				continue
			}
			if !integrationIsFieldName(name) {
				t.Errorf("%s:%d: %q is not a valid field name, in line %q", path, lineNo, name, line)
			}
			if strings.TrimSpace(value) == "" {
				t.Errorf("%s:%d: field %q has an empty value; empty fields must not be emitted at all",
					path, lineNo, name)
			}
		}

		// The generated index must survive the parser it was produced from.
		reparsed, err := parsePackages(strings.NewReader(content), arch, fixtureMirror)
		if err != nil {
			t.Fatalf("parsePackages(%s): %v", path, err)
		}
		if len(reparsed) != len(selected[arch]) {
			t.Fatalf("%s: re-parsing the generated index yields %d packages, want the %d that were written",
				path, len(reparsed), len(selected[arch]))
		}

		// Every field apt needs must survive the round trip unchanged. The
		// comparison is against what was selected in this same run, and it is
		// per field on purpose: parsePackages falls back to the index
		// architecture when "Architecture:" is absent, so a dropped field would
		// be silently reconstructed by a non-empty check alone.
		for i, pkg := range reparsed {
			want := selected[arch][i]

			if pkg.Package != want.Package {
				t.Errorf("%s: package %d re-parses as %q, want %q", path, i, pkg.Package, want.Package)
				continue
			}
			if pkg.Version != want.Version || pkg.Arch != want.Arch ||
				pkg.Filename != want.Filename || pkg.Size != want.Size ||
				pkg.MD5sum != want.MD5sum || pkg.SHA256 != want.SHA256 {
				t.Errorf("%s: package %q re-parses as version=%q architecture=%q filename=%q size=%q md5=%q sha256=%q, want version=%q architecture=%q filename=%q size=%q md5=%q sha256=%q",
					path, pkg.Package,
					pkg.Version, pkg.Arch, pkg.Filename, pkg.Size, pkg.MD5sum, pkg.SHA256,
					want.Version, want.Arch, want.Filename, want.Size, want.MD5sum, want.SHA256)
			}
		}

		// Without these fields apt cannot fetch or verify a package.
		for _, pkg := range reparsed {
			if pkg.Package == "" || pkg.Version == "" || pkg.Arch == "" ||
				pkg.Filename == "" || pkg.Size == "" || pkg.SHA256 == "" {
				t.Errorf("%s: package %q is missing a mandatory field: version=%q architecture=%q filename=%q size=%q sha256=%q",
					path, pkg.Package, pkg.Version, pkg.Arch, pkg.Filename, pkg.Size, pkg.SHA256)
			}
			// Filename is resolved against the repository root, so it must stay
			// a relative, forward-slashed pool path.
			if strings.Contains(pkg.Filename, `\`) {
				t.Errorf("%s: package %q has Filename %q, which must use forward slashes",
					path, pkg.Package, pkg.Filename)
			}
			if strings.HasPrefix(pkg.Filename, "/") || strings.Contains(pkg.Filename, "://") {
				t.Errorf("%s: package %q has Filename %q, which must be relative to the repository root",
					path, pkg.Package, pkg.Filename)
			}
		}
	}
}

// Regenerating from the same cached index must not churn the repository: apt
// clients would re-download every index for nothing, and a diff that is not
// reproducible hides real changes.
func TestIntegrationRegeneratingProducesTheSameIndex(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	_, first := integrationBuildRepo(t, config)

	if len(first.Entries) == 0 {
		t.Fatalf("generateDists produced no file for %s", first.Root)
	}

	firstFiles := map[string][]byte{}
	for _, entry := range first.Entries {
		data, err := os.ReadFile(entry.Abs)
		if err != nil {
			t.Fatalf("read %s: %v", entry.Abs, err)
		}
		firstFiles[entry.Rel] = data
	}
	firstRelease := readFile(t, filepath.Join(first.Root, "Release"))

	// Same destination, same cached index, second run.
	_, second := integrationBuildRepo(t, config)

	if len(second.Entries) != len(first.Entries) {
		t.Errorf("the second run generated %d files, want the %d of the first run",
			len(second.Entries), len(first.Entries))
	}

	for _, entry := range second.Entries {
		want, ok := firstFiles[entry.Rel]
		if !ok {
			t.Errorf("the second run generated %q, which the first run did not", entry.Rel)
			continue
		}

		got, err := os.ReadFile(entry.Abs)
		if err != nil {
			t.Errorf("read %s: %v", entry.Abs, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s changed between two runs over the same cached index: %d bytes then %d bytes",
				entry.Rel, len(want), len(got))
		}
	}

	// Release may differ in its Date stamp and in nothing else.
	withoutDate := func(release string) string {
		var kept []string
		for _, line := range strings.Split(release, "\n") {
			if strings.HasPrefix(line, "Date: ") {
				continue
			}
			kept = append(kept, line)
		}
		return strings.Join(kept, "\n")
	}

	secondRelease := readFile(t, filepath.Join(second.Root, "Release"))
	if withoutDate(secondRelease) != withoutDate(firstRelease) {
		t.Errorf("Release changed between two runs over the same cached index, in more than its Date:\n--- first ---\n%s\n--- second ---\n%s",
			firstRelease, secondRelease)
	}
}
