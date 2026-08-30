package tinyrepo

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ── file name parsing ───────────────────────────────────────────────────────

func TestParsePackageFileDebian(t *testing.T) {
	name, version, arch := parsePackageFile("nano_7.2-1_amd64.deb")

	if name != "nano" || version != "7.2-1" || arch != "amd64" {
		t.Errorf("got (%q, %q, %q), want (nano, 7.2-1, amd64)", name, version, arch)
	}
}

// An epoch cannot appear as a colon in a pool path, so Debian percent-encodes
// it. Showing "%3a" to the user would be showing them the transport, not the
// version.
func TestParsePackageFileDecodesADebianEpoch(t *testing.T) {
	_, version, _ := parsePackageFile("tzdata_2024a%3a1.0_all.deb")

	if version != "2024a:1.0" {
		t.Errorf("version = %q, want the epoch decoded", version)
	}
}

// A pacman package name may contain hyphens, so the name cannot be taken as
// "everything before the first hyphen".
func TestParsePackageFileArch(t *testing.T) {
	tests := map[string][3]string{
		"nano-7.2-1-x86_64.pkg.tar.zst":              {"nano", "7.2-1", "x86_64"},
		"python-pyqt5-5.15.10-3-x86_64.pkg.tar.zst":  {"python-pyqt5", "5.15.10-3", "x86_64"},
		"lz4-1:1.10.0-2-x86_64.pkg.tar.zst":          {"lz4", "1:1.10.0-2", "x86_64"},
		"ca-certificates-20240618-1-any.pkg.tar.zst": {"ca-certificates", "20240618-1", "any"},
		"nano-7.2-1-x86_64.pkg.tar.xz":               {"nano", "7.2-1", "x86_64"},
	}

	for fileName, want := range tests {
		name, version, arch := parsePackageFile(fileName)
		if name != want[0] || version != want[1] || arch != want[2] {
			t.Errorf("%s: got (%q, %q, %q), want %q", fileName, name, version, arch, want)
		}
	}
}

// A file that follows neither convention must be reported as unparseable, so
// the caller lists it under its own name rather than inventing one.
func TestParsePackageFileRejectsSomethingElse(t *testing.T) {
	for _, fileName := range []string{
		"Packages.gz", "nano.deb", "nano_7.2-1.deb", "README", "nano-7.2.pkg.tar.zst",
	} {
		if name, _, _ := parsePackageFile(fileName); name != "" {
			t.Errorf("%s was parsed as %q", fileName, name)
		}
	}
}

func TestIsPackageFile(t *testing.T) {
	yes := []string{"a.deb", "a.pkg.tar.zst", "a.pkg.tar.xz", "a.pkg.tar.gz"}
	no := []string{"Packages", "Packages.gz", "Release", "tinyrepo.db", "a.deb.tinyrepo.tmp"}

	for _, name := range yes {
		if !isPackageFile(name) {
			t.Errorf("%q was not recognised as a package", name)
		}
	}
	for _, name := range no {
		// The temporary is caught by hiddenName rather than the suffix list,
		// which scanPackages applies as well; here only the suffix matters.
		if name != "a.deb.tinyrepo.tmp" && isPackageFile(name) {
			t.Errorf("%q was recognised as a package", name)
		}
	}
}

// ── scanning ────────────────────────────────────────────────────────────────

// seedInventory builds a repository holding both packages and the metadata that
// must not be listed as one.
func seedInventory(t *testing.T, extra map[string]string) string {
	t.Helper()

	root := t.TempDir()
	files := map[string]string{
		"pool/main/n/nano/nano_7.2-1_amd64.deb":     "nano package body",
		"pool/main/w/wget/wget_1.21-3_amd64.deb":    "wget package body, longer",
		"pool/main/c/curl/curl_7.88-1_amd64.deb":    "curl",
		"dists/tinyrepo/main/binary-amd64/Packages": "Package: nano\n",
		"dists/tinyrepo/Release":                    "Origin: tinyrepo\n",
		// Must be skipped: the upstream cache, tinyrepo's state, and a
		// half-written download.
		"dists_cache/bookworm/main/binary-amd64/Packages":    "Package: nano\n",
		"dists_cache/pool/should-not-be-listed_1_amd64.deb":  "cached",
		".tinyrepo/manifest.tsv":                             "# manifest\n",
		"pool/main/n/nano/nano_7.3-1_amd64.deb.tinyrepo.tmp": "still downloading",
	}
	for path, body := range extra {
		files[path] = body
	}

	for path, body := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScanPackagesFindsOnlyPackages(t *testing.T) {
	root := seedInventory(t, nil)

	items, err := scanPackages(root, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(items) != 3 {
		names := make([]string, len(items))
		for i, item := range items {
			names[i] = item.RelPath
		}
		t.Fatalf("found %d packages (%v), want 3", len(items), names)
	}

	for _, item := range items {
		if strings.Contains(item.RelPath, "dists_cache") {
			t.Errorf("the upstream cache was listed: %s", item.RelPath)
		}
		if strings.HasSuffix(item.RelPath, TempSuffix) {
			t.Errorf("a half-written download was listed: %s", item.RelPath)
		}
	}
}

func TestScanPackagesReadsTheFileDetails(t *testing.T) {
	root := seedInventory(t, nil)

	items, err := scanPackages(root, nil)
	if err != nil {
		t.Fatal(err)
	}

	var nano inventoryItem
	for _, item := range items {
		if item.Name == "nano" {
			nano = item
		}
	}

	if nano.Version != "7.2-1" || nano.Arch != "amd64" {
		t.Errorf("nano parsed as %q %q", nano.Version, nano.Arch)
	}
	if nano.Size != int64(len("nano package body")) {
		t.Errorf("size = %d, want the file size", nano.Size)
	}
	if nano.Modified.IsZero() {
		t.Error("no modification time was read")
	}
	if nano.RelPath != "pool/main/n/nano/nano_7.2-1_amd64.deb" {
		t.Errorf("relPath = %q", nano.RelPath)
	}
}

// The Declared flag is what tells the two buttons apart, and what -cl acts on.
func TestScanPackagesMarksWhatIsDeclared(t *testing.T) {
	root := seedInventory(t, nil)

	items, err := scanPackages(root, []string{"nano"})
	if err != nil {
		t.Fatal(err)
	}

	for _, item := range items {
		want := item.Name == "nano"
		if item.Declared != want {
			t.Errorf("%s: declared = %v, want %v", item.Name, item.Declared, want)
		}
	}
}

func TestScanPackagesIsSorted(t *testing.T) {
	root := seedInventory(t, nil)

	items, _ := scanPackages(root, nil)

	for i := 1; i < len(items); i++ {
		if items[i-1].Name > items[i].Name {
			t.Errorf("not sorted: %q before %q", items[i-1].Name, items[i].Name)
		}
	}
}

// A package file with a name neither convention explains still occupies disk,
// so it has to be listed rather than silently dropped.
func TestScanPackagesListsAnUnparseableFile(t *testing.T) {
	root := seedInventory(t, map[string]string{"pool/strange.deb": "x"})

	items, _ := scanPackages(root, nil)

	found := false
	for _, item := range items {
		if item.Name == "strange.deb" {
			found = true
		}
	}
	if !found {
		t.Error("a package file that could not be parsed was not listed")
	}
}

// ── paging and filtering ────────────────────────────────────────────────────

func manyItems(n int) []inventoryItem {
	items := make([]inventoryItem, n)
	for i := range items {
		items[i] = inventoryItem{Name: string(rune('a'+i%26)) + "pkg", Size: 100}
	}
	return items
}

func TestPaginateCutsPages(t *testing.T) {
	items := manyItems(120)

	first := paginate(items, inventoryFilter{}, 1)
	if len(first.Items) != pageSize {
		t.Errorf("page 1 has %d rows, want %d", len(first.Items), pageSize)
	}
	if first.Pages != 3 {
		t.Errorf("pages = %d, want 3", first.Pages)
	}
	if first.Total != 120 || first.Files != 120 {
		t.Errorf("total = %d, files = %d, want 120", first.Total, first.Files)
	}
	if first.HasPrev() || !first.HasNext() {
		t.Error("page 1 should have a next page and no previous")
	}

	last := paginate(items, inventoryFilter{}, 3)
	if len(last.Items) != 20 {
		t.Errorf("last page has %d rows, want 20", len(last.Items))
	}
	if !last.HasPrev() || last.HasNext() {
		t.Error("the last page should have a previous page and no next")
	}
}

// A stale link should land on the nearest page rather than on an error.
func TestPaginateClampsOutOfRangePages(t *testing.T) {
	items := manyItems(60)

	if got := paginate(items, inventoryFilter{}, 99); got.Page != 2 {
		t.Errorf("page = %d, want it clamped to 2", got.Page)
	}
	if got := paginate(items, inventoryFilter{}, -5); got.Page != 1 {
		t.Errorf("page = %d, want it clamped to 1", got.Page)
	}
}

func TestPaginateOnAnEmptyRepository(t *testing.T) {
	got := paginate(nil, inventoryFilter{}, 1)

	if got.Pages != 1 || got.Total != 0 || len(got.Items) != 0 {
		t.Errorf("empty repository paginated as %+v", got)
	}
}

func TestPaginateFilters(t *testing.T) {
	items := []inventoryItem{
		{Name: "nano", Size: 10, Declared: true},
		{Name: "wget", Size: 20},
		{Name: "nano-tiny", Size: 30},
	}

	byName := paginate(items, inventoryFilter{Query: "NAN"}, 1)
	if byName.Total != 2 {
		t.Errorf("query matched %d, want 2 (case-insensitive substring)", byName.Total)
	}

	undeclared := paginate(items, inventoryFilter{Undeclared: true}, 1)
	if undeclared.Total != 2 {
		t.Errorf("undeclared matched %d, want 2", undeclared.Total)
	}
	for _, item := range undeclared.Items {
		if item.Declared {
			t.Error("a declared package showed up in the undeclared filter")
		}
	}

	// The size line describes the whole repository, not the filtered view, so
	// it does not change as the filter narrows.
	if byName.Files != 3 || byName.Bytes != 60 {
		t.Errorf("files = %d, bytes = %d, want the whole repository", byName.Files, byName.Bytes)
	}
}

// Paging links have to keep the filter, or page 2 of a search is page 2 of
// everything.
func TestPageHrefKeepsTheFilter(t *testing.T) {
	page := inventoryPage{Filter: inventoryFilter{Query: "nano", Undeclared: true}}

	got := string(page.PageHref(2))
	for _, want := range []string{"page=2", "q=nano", "undeclared=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("PageHref = %q, want %q in it", got, want)
		}
	}
	if !strings.HasPrefix(got, adminPath+"/packages?") {
		t.Errorf("PageHref = %q", got)
	}

	plain := string((inventoryPage{}).PageHref(1))
	if plain != adminPath+"/packages?page=1" {
		t.Errorf("PageHref = %q for no filter", plain)
	}
}

// A pacman epoch puts a colon in the file name. In a relative URL that would
// read as a scheme, so the link has to be rooted; once it is, the colon needs
// no escaping and is left readable.
func TestInventoryHrefIsRooted(t *testing.T) {
	item := inventoryItem{RelPath: "lz4-1:1.10.0-2-x86_64.pkg.tar.zst"}

	href := item.Href()
	if !strings.HasPrefix(href, "/") {
		t.Errorf("Href = %q, want it rooted so the epoch is not read as a scheme", href)
	}
	if href != "/lz4-1:1.10.0-2-x86_64.pkg.tar.zst" {
		t.Errorf("Href = %q", href)
	}
}

// The separators have to survive, and the characters that would end the path
// early have to not.
func TestInventoryHrefEscapesWhatWouldBreakThePath(t *testing.T) {
	nested := inventoryItem{RelPath: "pool/main/n/nano/nano_7.2-1_amd64.deb"}
	if got := nested.Href(); got != "/pool/main/n/nano/nano_7.2-1_amd64.deb" {
		t.Errorf("Href = %q, want the separators kept", got)
	}

	odd := inventoryItem{RelPath: "pool/we ird/a#b?c_1_all.deb"}
	href := odd.Href()

	for _, bad := range []string{" ", "#", "?"} {
		if strings.Contains(href, bad) {
			t.Errorf("Href = %q, want %q escaped", href, bad)
		}
	}
	if !strings.Contains(href, "/pool/") {
		t.Errorf("Href = %q, want the directory separators kept", href)
	}
}

// ── declaring ───────────────────────────────────────────────────────────────

func declareFixture(t *testing.T, packages ...string) string {
	t.Helper()

	dir := t.TempDir()
	config := defaultConfig()
	config.Server.Source = []string{"https://deb.debian.org/debian bookworm main"}
	config.Destination.Path = dir
	config.Destination.Packages = packages

	path := filepath.Join(dir, "config.toml")
	if err := saveConfigFile(path, &config); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDeclarePackageAddsAndRemoves(t *testing.T) {
	path := declareFixture(t, "nano")

	if err := declarePackage(path, "wget", true); err != nil {
		t.Fatal(err)
	}
	config, _ := loadConfigFile(path)
	if len(config.Destination.Packages) != 2 {
		t.Fatalf("packages = %v, want nano and wget", config.Destination.Packages)
	}
	// Sorted, so the list stays readable however it was added to.
	if config.Destination.Packages[0] != "nano" || config.Destination.Packages[1] != "wget" {
		t.Errorf("packages = %v, want them sorted", config.Destination.Packages)
	}

	if err := declarePackage(path, "nano", false); err != nil {
		t.Fatal(err)
	}
	config, _ = loadConfigFile(path)
	if len(config.Destination.Packages) != 1 || config.Destination.Packages[0] != "wget" {
		t.Errorf("packages = %v, want only wget", config.Destination.Packages)
	}
}

// Adding what is already there, or removing what is not, must not duplicate an
// entry or fail: a double-clicked button is a normal thing to happen.
func TestDeclarePackageIsIdempotent(t *testing.T) {
	path := declareFixture(t, "nano")

	for range 3 {
		if err := declarePackage(path, "nano", true); err != nil {
			t.Fatal(err)
		}
	}
	config, _ := loadConfigFile(path)
	if len(config.Destination.Packages) != 1 {
		t.Errorf("packages = %v, want no duplicate", config.Destination.Packages)
	}

	for range 3 {
		if err := declarePackage(path, "never-there", false); err != nil {
			t.Fatal(err)
		}
	}
	config, _ = loadConfigFile(path)
	if len(config.Destination.Packages) != 1 {
		t.Errorf("packages = %v, want it unchanged", config.Destination.Packages)
	}
}

// Removing the last package leaves an empty list, which is valid: it is what
// -cl reads as "keep nothing".
func TestDeclarePackageCanEmptyTheList(t *testing.T) {
	path := declareFixture(t, "nano")

	if err := declarePackage(path, "nano", false); err != nil {
		t.Fatal(err)
	}

	config, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Destination.Packages) != 0 {
		t.Errorf("packages = %v, want empty", config.Destination.Packages)
	}
}

// ── the page ────────────────────────────────────────────────────────────────

func TestPackagesPageNeedsASession(t *testing.T) {
	f := newAdminFixture(t)

	if response := f.get(adminPath + "/packages"); response.StatusCode != http.StatusSeeOther {
		t.Errorf("got %d, want a redirect to the login page", response.StatusCode)
	}
}

func TestPackagesPageWriteNeedsTheCSRFToken(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	response := f.post(adminPath+"/packages", url.Values{
		"name": {"nano"}, "declare": {"add"}})
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("got %d, want 403", response.StatusCode)
	}

	config := f.savedConfig()
	for _, name := range config.Destination.Packages {
		if name == "nano" {
			// The fixture starts with nano declared, so check it another way:
			// nothing should have been added.
		}
	}
	if len(config.Destination.Packages) != 1 {
		t.Errorf("packages = %v, want the list untouched", config.Destination.Packages)
	}
}

func TestPackagesPageListsWhatIsOnDisk(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	root := f.savedConfig().Destination.Path
	deb := filepath.Join(root, "pool", "main", "w", "wget", "wget_1.21-3_amd64.deb")
	if err := os.MkdirAll(filepath.Dir(deb), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deb, []byte("wget body"), 0644); err != nil {
		t.Fatal(err)
	}

	body := f.body(f.get(adminPath + "/packages"))

	for _, want := range []string{"wget", "1.21-3", "amd64",
		"/pool/main/w/wget/wget_1.21-3_amd64.deb"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
	// wget is not in the fixture's package list, so it is offered for adoption.
	if !strings.Contains(body, `value="add"`) {
		t.Error("an undeclared package was not offered an add button")
	}
}

func TestPackagesPageAdoptsAPackage(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()

	response := f.post(adminPath+"/packages", url.Values{
		"csrf": {csrf}, "name": {"wget"}, "declare": {"add"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", response.StatusCode)
	}

	config := f.savedConfig()
	found := false
	for _, name := range config.Destination.Packages {
		if name == "wget" {
			found = true
		}
	}
	if !found {
		t.Errorf("packages = %v, want wget added", config.Destination.Packages)
	}
}

func TestPackagesPageDropsAPackage(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()

	response := f.post(adminPath+"/packages", url.Values{
		"csrf": {csrf}, "name": {"nano"}, "declare": {"remove"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", response.StatusCode)
	}

	if names := f.savedConfig().Destination.Packages; len(names) != 0 {
		t.Errorf("packages = %v, want nano removed", names)
	}
}

func TestPackagesPageRefusesAnEmptyName(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()

	response := f.post(adminPath+"/packages", url.Values{
		"csrf": {csrf}, "name": {""}, "declare": {"add"}})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d, want 400", response.StatusCode)
	}
}

// An empty repository is a normal state, not an error page.
func TestPackagesPageOnAnEmptyRepository(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	response := f.get(adminPath + "/packages")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", response.StatusCode)
	}
}

// The rendered page is the only place the escaping bug showed: a "&a=b" tail
// interpolated into href="...?page={{.}}{{.}}" is escaped by html/template as a
// single parameter value, so the filter arrives as one nonsensical parameter.
func TestPagerLinksSurviveTemplateEscaping(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	root := f.savedConfig().Destination.Path
	dir := filepath.Join(root, "pool", "test")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// Enough files to produce a second page.
	for i := range pageSize + 5 {
		name := fmt.Sprintf("fake-pkg-%d_1.0-1_amd64.deb", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	body := f.body(f.get(adminPath + "/packages?q=fake&undeclared=1"))

	if !strings.Contains(body, "page=2&amp;q=fake") {
		t.Errorf("the next-page link lost its filter, or the separator was escaped:\n%s",
			pagerLinks(body))
	}
	if strings.Contains(body, "%26") || strings.Contains(body, "%3d") {
		t.Errorf("a paging link has an escaped separator:\n%s", pagerLinks(body))
	}
}

func pagerLinks(body string) string {
	var found []string
	for _, match := range regexp.MustCompile(`href="[^"]*packages\?[^"]*"`).FindAllString(body, -1) {
		found = append(found, match)
	}
	return strings.Join(found, "\n")
}

// The listing has to scroll inside its own box rather than stretch the page:
// with 50 rows and a table wider than the column, a page-level scroll pushes
// the pager and the filter out of reach.
func TestPackagesTableScrollsInItsOwnBox(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	body := f.body(f.get(adminPath + "/packages"))

	if !strings.Contains(body, `<div class="scroll tall">`) {
		t.Error("the listing table is not inside a scroll box")
	}

	// The rules that make the box actually scroll, rather than let the table
	// squeeze itself into the container or run the page long.
	for _, rule := range []string{
		".scroll{overflow:auto",                // both axes, on the box
		".scroll.tall{max-height:",             // so the page keeps its height
		".scroll table{min-width:max-content}", // so the table overflows instead of shrinking
		".scroll thead th{position:sticky",     // the header stays put while rows move
	} {
		if !strings.Contains(body, rule) {
			t.Errorf("the stylesheet is missing %q", rule)
		}
	}
}

// Both pagers are rendered whatever the page count, so the controls are
// reachable without scrolling past the rows.
func TestPackagesPageHasBothPagers(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	body := f.body(f.get(adminPath + "/packages"))

	if got := strings.Count(body, `class="pager"`); got != 2 {
		t.Errorf("found %d pagers, want one above and one below the table", got)
	}
}
