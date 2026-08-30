package tinyrepo

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func availableTestItems() []availableItem {
	return []availableItem{
		{Name: "nano", Version: "7.2-1", Arch: "amd64", Size: 1000, Description: "small, friendly text editor"},
		{Name: "wget", Version: "1.21-3", Arch: "amd64", Size: 2000, Description: "retrieves files from the web"},
		{Name: "curl", Version: "7.88-1", Arch: "amd64", Size: 3000, Description: "command line tool for transferring data"},
	}
}

func TestAvailableFilterMatchesNameAndDescription(t *testing.T) {
	items := availableTestItems()

	byName := paginateAvailable(items, nil, availableFilter{Query: "NAN"}, 1)
	if byName.Total != 1 || byName.Items[0].Name != "nano" {
		t.Errorf("name search matched %d", byName.Total)
	}

	// Half the time you know what the package does, not what it is called.
	byText := paginateAvailable(items, nil, availableFilter{Query: "transferring"}, 1)
	if byText.Total != 1 || byText.Items[0].Name != "curl" {
		t.Errorf("description search matched %d", byText.Total)
	}
}

func TestAvailableMarksWhatIsDeclared(t *testing.T) {
	page := paginateAvailable(availableTestItems(), []string{"nano"}, availableFilter{}, 1)

	for _, item := range page.Items {
		want := item.Name == "nano"
		if item.Declared != want {
			t.Errorf("%s: declared = %v, want %v", item.Name, item.Declared, want)
		}
	}

	only := paginateAvailable(availableTestItems(), []string{"nano"}, availableFilter{Declared: true}, 1)
	if only.Total != 1 {
		t.Errorf("the declared filter matched %d, want 1", only.Total)
	}
	// The catalog count describes the mirror, not the filtered view.
	if only.Catalog != 3 {
		t.Errorf("catalog = %d, want 3", only.Catalog)
	}
}

func TestAvailablePaginates(t *testing.T) {
	items := make([]availableItem, pageSize*2+7)
	for i := range items {
		items[i] = availableItem{Name: "pkg", Arch: "amd64"}
	}

	first := paginateAvailable(items, nil, availableFilter{}, 1)
	if len(first.Items) != pageSize || first.Pages != 3 {
		t.Errorf("page 1: %d rows, %d pages", len(first.Items), first.Pages)
	}

	if clamped := paginateAvailable(items, nil, availableFilter{}, 99); clamped.Page != 3 {
		t.Errorf("page = %d, want it clamped to 3", clamped.Page)
	}
}

func TestAvailablePageHrefKeepsTheFilter(t *testing.T) {
	page := availablePage{Filter: availableFilter{Query: "nano", Declared: true}}

	got := string(page.PageHref(2))
	for _, want := range []string{"page=2", "q=nano", "declared=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("PageHref = %q, want %q in it", got, want)
		}
	}
	if !strings.HasPrefix(got, adminPath+"/available?") {
		t.Errorf("PageHref = %q", got)
	}
}

// A Debian Description is a one line synopsis followed by an indented body, and
// only the synopsis belongs in a table cell.
func TestShortDescription(t *testing.T) {
	full := "small, friendly text editor\n GNU nano is an easy-to-use text editor\n .\n It is a clone of Pico."

	if got := shortDescription(full); got != "small, friendly text editor" {
		t.Errorf("shortDescription = %q", got)
	}
	if got := shortDescription(""); got != "" {
		t.Errorf("shortDescription = %q for an empty description", got)
	}
}

// ── the cache ───────────────────────────────────────────────────────────────

// seedIndexCache writes a cached Packages index the loader will read, laid out
// the way -di leaves it: the index under binary-<arch>, and the url_base.txt
// beside the dist that records which mirror it came from.
func seedIndexCache(t *testing.T, root, body string) string {
	t.Helper()

	dist := filepath.Join(root, DistsCacheName, "bookworm")
	path := filepath.Join(dist, "main", "binary-amd64", "Packages")

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(dist, DistsCacheNameUrlBase)
	if err := os.WriteFile(base, []byte("https://deb.debian.org/debian\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

const availableIndexBody = `Package: nano
Version: 7.2-1
Architecture: amd64
Size: 1000
Description: small, friendly text editor
 The long part, which must not reach the table.
Filename: pool/main/n/nano/nano_7.2-1_amd64.deb

Package: wget
Version: 1.21-3
Architecture: amd64
Size: 2000
Description: retrieves files from the web
Filename: pool/main/w/wget/wget_1.21-3_amd64.deb

`

func availableTestConfig(t *testing.T) *Config {
	t.Helper()

	config := defaultConfig()
	config.Server.Source = []string{"https://deb.debian.org/debian bookworm main"}
	config.Destination.Path = t.TempDir()
	config.Destination.Arch = []string{"amd64"}
	return &config
}

func TestAvailableReadsTheCachedIndex(t *testing.T) {
	config := availableTestConfig(t)
	seedIndexCache(t, config.Destination.Path, availableIndexBody)

	items, err := readAvailable(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("read %d packages, want 2", len(items))
	}

	// Sorted, so the same index always lists the same way.
	if items[0].Name != "nano" || items[1].Name != "wget" {
		t.Errorf("got %q and %q", items[0].Name, items[1].Name)
	}
	if items[0].Size != 1000 {
		t.Errorf("size = %d, want 1000", items[0].Size)
	}
	if items[0].Description != "small, friendly text editor" {
		t.Errorf("description = %q, want only the synopsis", items[0].Description)
	}
}

// Without an index there is nothing to list, and the page has to say so rather
// than show an empty table as if the mirror were empty.
func TestAvailableWithoutAnIndexIsAnError(t *testing.T) {
	config := availableTestConfig(t)

	cache := &availableCache{}
	if _, err := cache.load(config); err == nil {
		t.Error("loading with no cached index reported success")
	}
}

// Parsing 50 MB of stanzas per request would make the page unusable, so the
// second load has to come from memory.
func TestAvailableCacheReusesTheParse(t *testing.T) {
	config := availableTestConfig(t)
	path := seedIndexCache(t, config.Destination.Path, availableIndexBody)

	cache := &availableCache{}
	first, err := cache.load(config)
	if err != nil {
		t.Fatal(err)
	}

	// Changing the file's contents while putting its timestamp back must not be
	// picked up: that is the trade the cache makes, and it is what proves the
	// second call did not re-read.
	stamp := newestIndexTime(config)

	if err := os.WriteFile(path, []byte("Package: something-else\nVersion: 1\n\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	second, err := cache.load(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first) {
		t.Errorf("the cache re-read the index: %d items then %d", len(first), len(second))
	}
}

// Running -di from the panel has to show up on the next page view, without a
// restart.
func TestAvailableCacheNoticesANewIndex(t *testing.T) {
	config := availableTestConfig(t)
	path := seedIndexCache(t, config.Destination.Path, availableIndexBody)

	cache := &availableCache{}
	if _, err := cache.load(config); err != nil {
		t.Fatal(err)
	}

	body := availableIndexBody + "Package: curl\nVersion: 7.88-1\nArchitecture: amd64\nSize: 3000\nFilename: pool/main/c/curl/curl_7.88-1_amd64.deb\n\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	items, err := cache.load(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Errorf("got %d packages after the index changed, want 3", len(items))
	}
}

// Pointing the config at another repository must not serve the previous one's
// catalog.
func TestAvailableCacheIsPerRepository(t *testing.T) {
	first := availableTestConfig(t)
	seedIndexCache(t, first.Destination.Path, availableIndexBody)

	cache := &availableCache{}
	if _, err := cache.load(first); err != nil {
		t.Fatal(err)
	}

	second := availableTestConfig(t)
	seedIndexCache(t, second.Destination.Path,
		"Package: curl\nVersion: 7.88-1\nArchitecture: amd64\nSize: 1\nFilename: pool/c.deb\n\n")

	items, err := cache.load(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "curl" {
		t.Errorf("got %d items (%v), want the second repository's catalog", len(items), items)
	}
}

// ── the page ────────────────────────────────────────────────────────────────

func TestAvailablePageNeedsASession(t *testing.T) {
	f := newAdminFixture(t)

	if response := f.get(adminPath + "/available"); response.StatusCode != http.StatusSeeOther {
		t.Errorf("got %d, want a redirect to the login page", response.StatusCode)
	}
}

func TestAvailablePageWriteNeedsTheCSRFToken(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	response := f.post(adminPath+"/available", url.Values{
		"name": {"wget"}, "declare": {"add"}})
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("got %d, want 403", response.StatusCode)
	}
	if len(f.savedConfig().Destination.Packages) != 1 {
		t.Error("a request without a token changed the list")
	}
}

// Without a cached index the page explains itself instead of looking empty.
func TestAvailablePageExplainsAMissingIndex(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	body := f.body(f.get(adminPath + "/available"))
	if !strings.Contains(body, "-di") {
		t.Error("the page does not say the index has to be downloaded first")
	}
}

func TestAvailablePageListsAndAdopts(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()

	seedIndexCache(t, f.savedConfig().Destination.Path, availableIndexBody)

	body := f.body(f.get(adminPath + "/available"))
	for _, want := range []string{"wget", "1.21-3", "retrieves files from the web"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}

	response := f.post(adminPath+"/available", url.Values{
		"csrf": {csrf}, "name": {"wget"}, "declare": {"add"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", response.StatusCode)
	}

	found := false
	for _, name := range f.savedConfig().Destination.Packages {
		if name == "wget" {
			found = true
		}
	}
	if !found {
		t.Error("the package was not added to the list")
	}
}

// ── navigation ──────────────────────────────────────────────────────────────

// Every page carries the same tabs, with the current one marked, so the panel's
// pages are discoverable rather than reachable only from the dashboard.
func TestEveryPanelPageHasTheNav(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	for path, current := range map[string]string{
		"/":          "/admin/\" aria-current",
		"/packages":  "/admin/packages\" aria-current",
		"/available": "/admin/available\" aria-current",
		"/log":       "/admin/log\" aria-current",
	} {
		body := f.body(f.get(adminPath + path))

		if !strings.Contains(body, `class="topnav"`) {
			t.Errorf("%s has no navigation", path)
			continue
		}
		for _, link := range []string{"/admin/", "/admin/packages", "/admin/available", "/admin/log"} {
			if !strings.Contains(body, `href="`+link+`"`) {
				t.Errorf("%s does not link to %s", path, link)
			}
		}
		if !strings.Contains(body, current) {
			t.Errorf("%s does not mark itself as the current page", path)
		}
	}
}

// The button is the point of the row, so it must not be the thing you have to
// scroll sideways to reach.
func TestActionColumnStaysPinned(t *testing.T) {
	f := newAdminFixture(t)
	f.login()
	seedIndexCache(t, f.savedConfig().Destination.Path, availableIndexBody)

	body := f.body(f.get(adminPath + "/available"))

	if !strings.Contains(body, ".scroll td.row-action,.scroll thead th:last-child{position:sticky") {
		t.Error("the action column is not pinned to the right edge of the scroll box")
	}
}
