package tinyrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Parser fidelity against 300+ real Debian stanzas.
// See integration_test.go for the shared fixture and scaffolding.

// parserFieldAccessors maps the Debian field names parseField recognises to the
// struct field each one must land in. It is a test oracle, not a second parser:
// no line splitting, no stanza logic, no fallback rules. It lets the tests below
// compare what the parser produced against the raw bytes of a real index without
// writing down a single upstream value.
var parserFieldAccessors = map[string]func(*PackageDeb) string{
	"Package":        func(p *PackageDeb) string { return p.Package },
	"Version":        func(p *PackageDeb) string { return p.Version },
	"Architecture":   func(p *PackageDeb) string { return p.Arch },
	"Installed-Size": func(p *PackageDeb) string { return p.InstalledSize },
	"Maintainer":     func(p *PackageDeb) string { return p.Maintainer },
	"Filename":       func(p *PackageDeb) string { return p.Filename },
	"Size":           func(p *PackageDeb) string { return p.Size },
	"MD5sum":         func(p *PackageDeb) string { return p.MD5sum },
	"SHA256":         func(p *PackageDeb) string { return p.SHA256 },
	"Pre-Depends":    func(p *PackageDeb) string { return p.Pre_Depends },
	"Depends":        func(p *PackageDeb) string { return p.Depends },
	"Breaks":         func(p *PackageDeb) string { return p.Breaks },
	"Homepage":       func(p *PackageDeb) string { return p.Homepage },
	"Description":    func(p *PackageDeb) string { return p.Description },
	"Tag":            func(p *PackageDeb) string { return p.Tag },
	"Section":        func(p *PackageDeb) string { return p.Section },
	"Priority":       func(p *PackageDeb) string { return p.Priority },
}

// parserRawStanza is one blank-line separated record of a real "Packages" file,
// read without going through the code under test.
type parserRawStanza struct {
	Name      string            // value of the "Package:" field, "" when absent
	HasFold   bool              // the record contains at least one continuation line
	Fields    map[string]string // recognised fields, last occurrence winning
	AfterFold map[string]string // recognised fields written after a continuation line
}

// parserScanStanzas splits a Packages file the way the format defines it:
// records separated by blank lines, fields at column zero, continuation lines
// indented. Only format rules are encoded here, never an upstream value.
func parserScanStanzas(t *testing.T, path string) []parserRawStanza {
	t.Helper()

	newStanza := func() parserRawStanza {
		return parserRawStanza{Fields: map[string]string{}, AfterFold: map[string]string{}}
	}

	var stanzas []parserRawStanza
	current := newStanza()
	inRecord := false

	flush := func() {
		if inRecord {
			stanzas = append(stanzas, current)
		}
		current = newStanza()
		inRecord = false
	}

	for _, line := range strings.Split(readFile(t, path), "\n") {
		// bufio.ScanLines drops a trailing carriage return; mirror it so the two
		// views of the same file cannot disagree over line endings.
		line = strings.TrimSuffix(line, "\r")

		if line == "" {
			flush()
			continue
		}
		inRecord = true

		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			current.HasFold = true
			continue
		}

		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if _, known := parserFieldAccessors[name]; !known {
			continue
		}

		current.Fields[name] = value
		if current.HasFold {
			current.AfterFold[name] = value
		}
		if name == "Package" {
			current.Name = value
		}
	}
	flush()

	return stanzas
}

// parserNamedStanzas keeps the records that actually declare a package, which
// are exactly the ones parsePackages is expected to return.
func parserNamedStanzas(stanzas []parserRawStanza) []parserRawStanza {
	var named []parserRawStanza
	for _, stanza := range stanzas {
		if stanza.Name != "" {
			named = append(named, stanza)
		}
	}
	return named
}

// parserStanzaLabel names a stanza in a failure message even when the Package
// field is itself what is broken.
func parserStanzaLabel(position int, p *PackageDeb) string {
	if p.Package != "" {
		return fmt.Sprintf("package %q (stanza %d)", p.Package, position)
	}
	return fmt.Sprintf("unnamed package at stanza %d (Filename %q)", position, p.Filename)
}

// parserRealIndex reads the cached fixture index through the production code
// path and returns the packages together with the path they came from.
func parserRealIndex(t *testing.T, config *Config) ([]*PackageDeb, string) {
	t.Helper()

	indexPath := cachedIndexPath(config)

	packages, err := readDebPackages(indexPath)
	if err != nil {
		t.Fatalf("readDebPackages(%s): %v", indexPath, err)
	}

	// A real component index is never nearly empty. Without this guard a
	// truncated download would make every other assertion vacuously true.
	if len(packages) < 100 {
		t.Fatalf("%s parsed to only %d packages, want more than 100: the fixture looks truncated",
			indexPath, len(packages))
	}
	return packages, indexPath
}

// The parser must return one package per record that names one: no more (a
// continuation line starting a phantom stanza) and no fewer (the final record
// being dropped for want of a trailing blank line).
func TestIntegrationParserCountsEveryRealStanza(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	packages, indexPath := parserRealIndex(t, config)

	raw := parserScanStanzas(t, indexPath)
	named := parserNamedStanzas(raw)

	if len(packages) != len(named) {
		switch {
		case len(packages) < len(named):
			t.Fatalf("parsed %d packages from %s but %d of its %d records name a package: %d stanza(s) were dropped",
				len(packages), indexPath, len(named), len(raw), len(named)-len(packages))
		default:
			t.Fatalf("parsed %d packages from %s but only %d of its %d records name a package: phantom stanzas were invented",
				len(packages), indexPath, len(named), len(raw))
		}
	}

	// Same count is not the same content: compare the names in order too, so a
	// dropped stanza compensated by a phantom one cannot pass.
	for i := range named {
		if packages[i].Package != named[i].Name {
			t.Fatalf("stanza %d: parser returned %q, the raw index names %q (in %s)",
				i, packages[i].Package, named[i].Name, indexPath)
		}
	}
}

// Every field tinyrepo needs in order to publish and fetch a package must
// survive the parser, for all 300-odd real stanzas.
func TestIntegrationEveryRealPackageCarriesTheRequiredFields(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	packages, indexPath := parserRealIndex(t, config)

	required := []string{"Package", "Version", "Architecture", "Filename", "Size", "SHA256"}

	for i, pkg := range packages {
		for _, field := range required {
			if parserFieldAccessors[field](pkg) == "" {
				t.Fatalf("%s: %s is empty, but %s publishes it for every package",
					parserStanzaLabel(i, pkg), field, indexPath)
			}
		}

		size, err := strconv.ParseInt(pkg.Size, 10, 64)
		if err != nil {
			t.Fatalf("%s: Size = %q is not an integer: %v (writeManifest would record 0 and downloadFile would stop checking the size)",
				parserStanzaLabel(i, pkg), pkg.Size, err)
		}
		if size <= 0 {
			t.Fatalf("%s: Size = %d, want a positive byte count", parserStanzaLabel(i, pkg), size)
		}

		// 32 bytes of hex is what SHA256 is, whatever upstream happens to
		// publish this month.
		if len(pkg.SHA256) != hex.EncodedLen(sha256.Size) {
			t.Fatalf("%s: SHA256 = %q is %d characters, want %d",
				parserStanzaLabel(i, pkg), pkg.SHA256, len(pkg.SHA256), hex.EncodedLen(sha256.Size))
		}
		if _, err := hex.DecodeString(pkg.SHA256); err != nil {
			t.Fatalf("%s: SHA256 = %q is not hexadecimal: %v", parserStanzaLabel(i, pkg), pkg.SHA256, err)
		}

		// downloadPool joins Filename onto the destination directory, so a
		// leading slash or a ".." segment would write outside the repository.
		if strings.HasPrefix(pkg.Filename, "/") {
			t.Fatalf("%s: Filename = %q is absolute, it would escape the destination directory",
				parserStanzaLabel(i, pkg), pkg.Filename)
		}
		if slices.Contains(strings.Split(pkg.Filename, "/"), "..") {
			t.Fatalf("%s: Filename = %q contains a %q segment, it would escape the destination directory",
				parserStanzaLabel(i, pkg), pkg.Filename, "..")
		}
	}
}

// A binary-amd64 index really does hold both "Architecture: all" and
// "Architecture: amd64" stanzas. This is the real-data proof that the parser
// keeps a package's own architecture while attributing it to the index it was
// read from, which is what makes an arch:all package land in every index.
func TestIntegrationRealIndexMixesArchAllAndNative(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	packages, indexPath := parserRealIndex(t, config)

	counts := map[string]int{}

	for i, pkg := range packages {
		counts[pkg.Arch]++

		if pkg.IndexArch != fixtureArch {
			t.Fatalf("%s: IndexArch = %q, want %q, derived from the binary-%s directory of %s",
				parserStanzaLabel(i, pkg), pkg.IndexArch, fixtureArch, fixtureArch, indexPath)
		}
		if pkg.Arch != "all" && pkg.Arch != fixtureArch {
			t.Errorf("%s: Architecture = %q, but a binary-%s index may only hold %q or \"all\"",
				parserStanzaLabel(i, pkg), pkg.Arch, fixtureArch, fixtureArch)
		}
	}

	if counts["all"] == 0 {
		t.Errorf("%s parsed to no \"Architecture: all\" package, so the arch:all path is not being exercised", indexPath)
	}
	if counts[fixtureArch] == 0 {
		t.Errorf("%s parsed to no %q package, so the native-architecture path is not being exercised", indexPath, fixtureArch)
	}

	// Cross-check the split against the raw bytes rather than a fixed number,
	// which upstream is free to change at any point release.
	rawAll := 0
	for _, stanza := range parserNamedStanzas(parserScanStanzas(t, indexPath)) {
		if stanza.Fields["Architecture"] == "all" {
			rawAll++
		}
	}
	if counts["all"] != rawAll {
		t.Errorf("parser reports %d arch:all packages, %s contains %d", counts["all"], indexPath, rawAll)
	}
}

// Field-by-field comparison of the parsed packages against the raw index: 300-odd
// real stanzas, every recognised field. Long fields are folded over several
// indented lines in the real archive, so this is also what proves a continuation
// line neither starts a stanza of its own nor costs the stanza the fields
// written after it.
func TestIntegrationParsedFieldsMatchTheRawIndex(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	packages, indexPath := parserRealIndex(t, config)

	named := parserNamedStanzas(parserScanStanzas(t, indexPath))
	if len(packages) != len(named) {
		t.Fatalf("parsed %d packages but %s holds %d records naming a package", len(packages), indexPath, len(named))
	}

	folded, afterFold := 0, 0

	// Compare by position rather than by name: two records may legitimately
	// carry the same Package name, and a name lookup would then silently
	// compare a stanza against the wrong package.
	for i, stanza := range named {
		pkg := packages[i]

		if pkg.Package != stanza.Name {
			t.Fatalf("stanza %d: parser returned %q, the raw index names %q", i, pkg.Package, stanza.Name)
		}
		if stanza.HasFold {
			folded++
			afterFold += len(stanza.AfterFold)
		}

		for name, want := range stanza.Fields {
			got := parserFieldAccessors[name](pkg)
			if got == want {
				continue
			}
			if _, follows := stanza.AfterFold[name]; follows {
				t.Errorf("%s: %s is written after a folded field and parsed as %q, %s has %q",
					parserStanzaLabel(i, pkg), name, got, indexPath, want)
				continue
			}
			t.Errorf("%s: %s parsed as %q, %s has %q", parserStanzaLabel(i, pkg), name, got, indexPath, want)
		}
	}

	// Informational only. Debian folds long Tag fields today and writes
	// Section, Priority, Filename, Size and the checksums after them, but the
	// test must not turn red if that ever changes upstream.
	t.Logf("compared %d stanzas of %s, %d of them fold a field, %d recognised fields follow a fold",
		len(named), indexPath, folded, afterFold)
	if folded == 0 {
		t.Logf("warning: %s no longer folds any field, so the continuation-line path is unexercised", indexPath)
	}
}

// The generated index must describe the packages it was built from. Parsing the
// real index, rendering it back with writeStanza and parsing the result again
// has to reproduce every field of every package.
func TestIntegrationWriteStanzaRoundTripsRealPackages(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	original, indexPath := parserRealIndex(t, config)

	baseURL, err := readURLBase(indexPath)
	if err != nil {
		t.Fatalf("readURLBase(%s): %v", indexPath, err)
	}

	var generated strings.Builder
	for _, pkg := range original {
		writeStanza(&generated, pkg)
	}

	reparsed, err := parsePackages(strings.NewReader(generated.String()), fixtureArch, baseURL)
	if err != nil {
		t.Fatalf("parsePackages of the generated index: %v", err)
	}

	if len(reparsed) != len(original) {
		t.Fatalf("the generated index parses back to %d packages, want the %d it was built from",
			len(reparsed), len(original))
	}

	fields := reflect.VisibleFields(reflect.TypeOf(PackageDeb{}))

	for _, field := range fields {
		// Comparing with reflect.Value.String() would silently succeed on a
		// non-string field, so make the test fail loudly instead of quietly
		// stopping to check it.
		if field.Type.Kind() != reflect.String {
			t.Fatalf("PackageDeb.%s is a %s, not a string: extend this test to compare it",
				field.Name, field.Type.Kind())
		}
	}

	for i := range original {
		before, after := original[i], reparsed[i]

		if before.Package != after.Package {
			t.Fatalf("position %d: %q became %q after the round trip, the stanza order changed",
				i, before.Package, after.Package)
		}

		beforeValue := reflect.ValueOf(*before)
		afterValue := reflect.ValueOf(*after)

		for _, field := range fields {
			want := beforeValue.FieldByIndex(field.Index).String()
			got := afterValue.FieldByIndex(field.Index).String()
			if got != want {
				t.Errorf("%s: %s came back as %q, want %q", parserStanzaLabel(i, before), field.Name, got, want)
			}
		}
	}
}

// Nothing in the format requires a trailing blank line, and a mirror that omits
// one must not cost the repository its last package. The real index does end
// with one, so this is the only coverage for the parser's final flush.
func TestIntegrationParsesLastRealStanzaWithoutTrailingNewline(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	complete, indexPath := parserRealIndex(t, config)

	// newRepo hands out a private copy of the cache, so truncating it here
	// cannot affect the shared download or any other test.
	trimmed := strings.TrimRight(readFile(t, indexPath), "\n")
	if err := os.WriteFile(indexPath, []byte(trimmed), 0644); err != nil {
		t.Fatalf("rewrite %s without its trailing blank line: %v", indexPath, err)
	}

	got, err := readDebPackages(indexPath)
	if err != nil {
		t.Fatalf("readDebPackages(%s): %v", indexPath, err)
	}

	want := complete[len(complete)-1]

	if len(got) != len(complete) {
		t.Fatalf("without a trailing blank line %s parsed to %d packages, want %d: the final stanza (%q) was dropped",
			indexPath, len(got), len(complete), want.Package)
	}

	last := got[len(got)-1]
	if last.Package != want.Package {
		t.Errorf("last package is %q, want %q", last.Package, want.Package)
	}
	// A half-flushed record would keep the name but lose the trailing fields.
	if last.Filename != want.Filename || last.SHA256 != want.SHA256 || last.Size != want.Size {
		t.Errorf("last package %q was flushed incomplete: Filename=%q Size=%q SHA256=%q, want %q %q %q",
			last.Package, last.Filename, last.Size, last.SHA256, want.Filename, want.Size, want.SHA256)
	}
}

// readDebPackages has to stamp every package with the mirror recorded in
// url_base.txt, because that is the only record of where a cached .deb must be
// fetched from once several sources are configured.
func TestIntegrationReadDebPackagesTakesTheMirrorFromURLBase(t *testing.T) {
	requireNetwork(t)

	config := newRepo(t)
	packages, indexPath := parserRealIndex(t, config)

	urlBase := filepath.Join(config.Destination.Path, DistsCacheName, fixtureDist, DistsCacheNameUrlBase)
	if !fileExists(urlBase) {
		t.Fatalf("downloadIndex did not record the mirror at %s", urlBase)
	}
	if got := strings.TrimSpace(readFile(t, urlBase)); got != fixtureMirror {
		t.Errorf("%s holds %q, want %q", urlBase, got, fixtureMirror)
	}

	for i, pkg := range packages {
		if pkg.FilenameUrl != fixtureMirror {
			t.Fatalf("%s: FilenameUrl = %q, want %q as recorded in %s",
				parserStanzaLabel(i, pkg), pkg.FilenameUrl, fixtureMirror, urlBase)
		}
	}

	// Prove the mirror is really read from that file rather than defaulted, and
	// that a recorded trailing slash is trimmed. The second mirror is a literal
	// this test writes into its own copy; it is never contacted.
	const otherMirror = "http://ftp.de.debian.org/debian/"
	if err := os.WriteFile(urlBase, []byte(otherMirror+"\n"), 0644); err != nil {
		t.Fatalf("rewrite %s: %v", urlBase, err)
	}

	moved, err := readDebPackages(indexPath)
	if err != nil {
		t.Fatalf("readDebPackages(%s): %v", indexPath, err)
	}
	if len(moved) != len(packages) {
		t.Fatalf("changing the mirror changed the package count: %d, want %d", len(moved), len(packages))
	}

	wantMirror := strings.TrimRight(otherMirror, "/")
	for i, pkg := range moved {
		if pkg.FilenameUrl != wantMirror {
			t.Fatalf("%s: FilenameUrl = %q after %s was rewritten to %q, want %q",
				parserStanzaLabel(i, pkg), pkg.FilenameUrl, urlBase, otherMirror, wantMirror)
		}
	}
}
