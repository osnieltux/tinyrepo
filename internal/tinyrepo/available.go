package tinyrepo

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The other half of the packages view: not what was downloaded, but everything
// the mirror offers, so a name can be found and added to the list without
// having to know it beforehand.
//
// This reads the index -di cached. A Debian suite is tens of thousands of
// stanzas and ~50 MB of text, so parsing it per request would make the page
// unusable; it is parsed once and kept until the cached index changes.

// availableItem is one package the mirror offers.
type availableItem struct {
	Name        string
	Version     string
	Arch        string
	Size        int64
	Description string

	// Declared says the name is already in [destination].packages.
	Declared bool
}

func (i availableItem) HumanSize() string {
	if i.Size <= 0 {
		return "-"
	}
	return humanSize(i.Size)
}

// availablePage is one rendered page of the catalog.
type availablePage struct {
	Items []availableItem

	Page  int
	Pages int
	Total int
	// Catalog is how many packages the mirror offers in total, so the header
	// says what is being searched rather than only what matched.
	Catalog int

	Filter availableFilter
	// Stale is set when the index has not been downloaded yet, which is the one
	// failure worth explaining rather than showing an empty table.
	Stale bool
}

func (p availablePage) HasPrev() bool { return p.Page > 1 }
func (p availablePage) HasNext() bool { return p.Page < p.Pages }
func (p availablePage) Prev() int     { return p.Page - 1 }
func (p availablePage) Next() int     { return p.Page + 1 }

// availableFilter is what the page was asked to show.
type availableFilter struct {
	Query string
	// Declared limits the list to names already in config.toml.
	Declared bool
}

func (f availableFilter) matches(item availableItem) bool {
	if f.Declared && !item.Declared {
		return false
	}
	if f.Query == "" {
		return true
	}

	query := strings.ToLower(f.Query)
	// The description too: half the time the name is what is being looked for
	// and half the time it is what the package does.
	return strings.Contains(strings.ToLower(item.Name), query) ||
		strings.Contains(strings.ToLower(item.Description), query)
}

// availableCache holds the parsed catalog between requests.
//
// Keyed on the newest modification time across the cached index files, so
// running -di from the panel is picked up on the next page view without a
// restart and without re-reading 50 MB on every click.
type availableCache struct {
	mu    sync.Mutex
	items []availableItem
	stamp time.Time
	root  string
	ready bool
}

// newestIndexTime is the most recent modification time of the cached upstream
// index. A zero time means nothing has been downloaded yet.
func newestIndexTime(config *Config) time.Time {
	var newest time.Time

	for _, dir := range []string{DistsCacheName, ArchCacheName} {
		root := filepath.Join(config.Destination.Path, dir)

		filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			if info, err := entry.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
	}
	return newest
}

// load returns the catalog, parsing it only when the index has changed since
// last time.
func (c *availableCache) load(config *Config) ([]availableItem, error) {
	stamp := newestIndexTime(config)
	if stamp.IsZero() {
		return nil, fmt.Errorf("%s", _t("err r indexes"))
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// The root is part of the key: pointing the config at another repository
	// must not serve the previous one's catalog.
	if c.ready && c.stamp.Equal(stamp) && c.root == config.Destination.Path {
		return c.items, nil
	}

	items, err := readAvailable(config)
	if err != nil {
		return nil, err
	}

	c.items = items
	c.stamp = stamp
	c.root = config.Destination.Path
	c.ready = true
	return items, nil
}

// readAvailable parses the cached index into the flat list the page shows.
func readAvailable(config *Config) ([]availableItem, error) {
	var items []availableItem

	switch config.Server.Type {
	case BackendArch:
		for _, arch := range config.Destination.Arch {
			index, err := loadArchIndexOpt(config, arch, false)
			if err != nil {
				return nil, err
			}
			for _, entry := range index.ByName {
				items = append(items, availableItem{
					Name:    entry.Name,
					Version: entry.Version,
					Arch:    arch,
					Size:    entry.Size,
				})
			}
		}

	default:
		byArch, err := loadDebIndex(config)
		if err != nil {
			return nil, err
		}
		for _, arch := range config.Destination.Arch {
			for _, pkg := range byArch[arch] {
				size, _ := strconv.ParseInt(pkg.Size, 10, 64)
				items = append(items, availableItem{
					Name:    pkg.Package,
					Version: pkg.Version,
					Arch:    arch,
					Size:    size,
					// The first line only: a Debian description runs for
					// paragraphs and this is one table cell.
					Description: shortDescription(pkg.Description),
				})
			}
		}
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("%s", _t("err r indexes"))
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Arch < items[j].Arch
	})
	return items, nil
}

// shortDescription is the synopsis: a Debian Description is a one line summary
// followed by an indented body, and only the first line belongs in a table.
func shortDescription(description string) string {
	first, _, _ := strings.Cut(description, "\n")
	return strings.TrimSpace(first)
}

// paginateAvailable applies the filter, marks what is already declared, and
// cuts out the requested page.
func paginateAvailable(items []availableItem, declared []string, filter availableFilter, page int) availablePage {
	wanted := map[string]bool{}
	for _, name := range declared {
		wanted[name] = true
	}

	out := availablePage{Page: page, Filter: filter, Catalog: len(items)}

	matched := make([]availableItem, 0, 128)
	for _, item := range items {
		item.Declared = wanted[item.Name]
		if filter.matches(item) {
			matched = append(matched, item)
		}
	}

	out.Total = len(matched)
	out.Pages = (out.Total + pageSize - 1) / pageSize
	if out.Pages == 0 {
		out.Pages = 1
	}
	if out.Page < 1 {
		out.Page = 1
	}
	if out.Page > out.Pages {
		out.Page = out.Pages
	}

	start := (out.Page - 1) * pageSize
	end := min(start+pageSize, out.Total)
	if start < out.Total {
		out.Items = matched[start:end]
	}
	return out
}

// PageHref is the link to one page of this listing, filter and all. Built here
// for the same reason inventoryPage's is: a query tail interpolated into an
// href is escaped as a single parameter value.
func (p availablePage) PageHref(page int) template.URL {
	values := url.Values{}
	values.Set("page", strconv.Itoa(page))

	if p.Filter.Query != "" {
		values.Set("q", p.Filter.Query)
	}
	if p.Filter.Declared {
		values.Set("declared", "1")
	}
	return template.URL(adminPath + "/available?" + values.Encode())
}

// declaredNames is the configured list, sorted, for the summary line.
func declaredNames(config *Config) []string {
	names := slices.Clone(config.Destination.Packages)
	slices.Sort(names)
	return names
}
