package tinyrepo

import (
	"html/template"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The panel's package list: what is actually on disk, as opposed to what
// config.toml asked for. On demand the two drift apart by design - a package
// arrives because a client asked for it, and nothing records that in the
// config - so being able to see the difference, and adopt or drop a name, is
// the point of the page.

// packageSuffixes are the files that are packages rather than repository
// metadata. Anything else under the destination path is an index, a database
// or a temporary, and none of those belong in this list.
var packageSuffixes = []string{".deb", ".pkg.tar.zst", ".pkg.tar.xz", ".pkg.tar.gz"}

// inventoryItem is one package file found in the repository.
type inventoryItem struct {
	// Name is the package name parsed out of the file name, which is what
	// [destination].packages holds.
	Name    string
	Version string
	Arch    string

	// RelPath is the path under the repository root, and so also the URL the
	// repository serves it at.
	RelPath  string
	Size     int64
	Modified time.Time

	// Declared says the name is in [destination].packages. The ones that are
	// not are what -cl would remove.
	Declared bool
}

// Href is the link that downloads this file from the running server.
//
// Rooted, and escaped segment by segment. Rooted because a package name can
// carry an epoch ("lz4-1:1.10.0-2-x86_64.pkg.tar.zst") and a bare colon in the
// first segment of a *relative* URL reads as a scheme; leading with "/" settles
// that, which is why the colon itself is left alone. The escaping is for the
// characters that would end the path instead: "?", "#" and a space.
func (i inventoryItem) Href() string {
	parts := strings.Split(i.RelPath, "/")
	for n, part := range parts {
		parts[n] = url.PathEscape(part)
	}
	return "/" + strings.Join(parts, "/")
}

// HumanSize renders the file size the way the directory listing does.
func (i inventoryItem) HumanSize() string { return humanSize(i.Size) }

// When renders the modification time to the minute, which is as much as a
// package file's timestamp is worth.
func (i inventoryItem) When() string { return i.Modified.Format("2006-01-02 15:04") }

// isPackageFile reports whether a file name is a package rather than metadata.
func isPackageFile(name string) bool {
	for _, suffix := range packageSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// parsePackageFile pulls the name, version and architecture out of a package
// file name. It returns the name empty when the file does not follow either
// convention, which is the caller's cue to list it without them rather than
// guess.
//
// Debian: <name>_<version>_<arch>.deb, with the version's epoch percent-encoded
// because a colon cannot appear in a pool path.
//
// pacman: <name>-<version>-<release>-<arch>.pkg.tar.zst, where the name itself
// may contain hyphens, so it is parsed from the right.
func parsePackageFile(fileName string) (name, version, arch string) {
	if strings.HasSuffix(fileName, ".deb") {
		base := strings.TrimSuffix(fileName, ".deb")

		parts := strings.Split(base, "_")
		if len(parts) != 3 {
			return "", "", ""
		}

		version = parts[1]
		// %3a is how an epoch survives a pool path; show it as the colon it is.
		if decoded, err := url.PathUnescape(version); err == nil {
			version = decoded
		}
		return parts[0], version, parts[2]
	}

	base := fileName
	for _, suffix := range packageSuffixes {
		if suffix != ".deb" && strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	if base == fileName {
		return "", "", ""
	}

	// name-version-release-arch, from the right: three hyphens are spoken for,
	// everything before them is the name.
	parts := strings.Split(base, "-")
	if len(parts) < 4 {
		return "", "", ""
	}

	arch = parts[len(parts)-1]
	version = strings.Join(parts[len(parts)-3:len(parts)-1], "-")
	name = strings.Join(parts[:len(parts)-3], "-")
	return name, version, arch
}

// scanPackages walks the repository and returns every package file in it.
//
// It reads the directory rather than the manifest: on demand the manifest
// describes what the mirror offers, which is the whole archive, while this page
// is about what is actually taking up space here.
func scanPackages(root string, declared []string) ([]inventoryItem, error) {
	wanted := map[string]bool{}
	for _, name := range declared {
		wanted[name] = true
	}

	var items []inventoryItem

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished under us, or one we cannot read, is
			// not a reason to fail the whole page.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if entry.IsDir() {
			// The caches and tinyrepo's own state hold no published packages,
			// and dists_cache in particular is large.
			if hiddenName(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}

		if !isPackageFile(entry.Name()) || hiddenName(entry.Name()) {
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		item := inventoryItem{RelPath: filepath.ToSlash(relative)}
		item.Name, item.Version, item.Arch = parsePackageFile(entry.Name())

		if item.Name == "" {
			// Unparseable, but still a file taking up space: show it under its
			// own name rather than hide it.
			item.Name = entry.Name()
		}
		item.Declared = wanted[item.Name]

		if info, err := entry.Info(); err == nil {
			item.Size = info.Size()
			item.Modified = info.ModTime()
		}

		items = append(items, item)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// By name, then version, so the same repository always lists the same way
	// and a package's versions sit together.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		if items[i].Arch != items[j].Arch {
			return items[i].Arch < items[j].Arch
		}
		return items[i].Version < items[j].Version
	})

	return items, nil
}

// inventoryFilter is what the page was asked to show.
type inventoryFilter struct {
	// Query matches the package name, case-insensitively. Without it a repo
	// built on demand is thousands of rows of nothing in particular.
	Query string
	// Undeclared limits the list to packages no longer in config.toml, which is
	// exactly the set -cl would remove.
	Undeclared bool
}

func (f inventoryFilter) matches(item inventoryItem) bool {
	if f.Undeclared && item.Declared {
		return false
	}
	if f.Query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(item.Name), strings.ToLower(f.Query))
}

// pageSize is how many rows a page shows. Large enough to be worth scrolling,
// small enough that the HTML stays small on a repository with tens of thousands
// of packages.
const pageSize = 50

// inventoryPage is one rendered page of the list.
type inventoryPage struct {
	Items []inventoryItem

	// Page is 1-indexed, as it appears in the URL.
	Page  int
	Pages int
	// Total is how many rows matched the filter; Files and Bytes describe the
	// whole repository, so the header can say what is on disk regardless of
	// what is being looked at.
	Total int
	Files int
	Bytes int64

	Filter inventoryFilter
}

// HumanBytes is the repository's size on disk.
func (p inventoryPage) HumanBytes() string { return humanSize(p.Bytes) }

func (p inventoryPage) HasPrev() bool { return p.Page > 1 }
func (p inventoryPage) HasNext() bool { return p.Page < p.Pages }
func (p inventoryPage) Prev() int     { return p.Page - 1 }
func (p inventoryPage) Next() int     { return p.Page + 1 }

// PageHref is the link to one page of this listing, filter and all.
//
// Built here and typed as template.URL rather than assembled in the template:
// interpolating a "&a=b&c=d" tail into href="...?page={{.}}{{.}}" puts it in
// the position of a single parameter value, so html/template escapes the "&"
// and "=" and the filter arrives as one nonsensical parameter. Every part below
// is escaped by url.Values, so the result is safe to mark as a URL.
func (p inventoryPage) PageHref(page int) template.URL {
	values := url.Values{}
	values.Set("page", strconv.Itoa(page))

	if p.Filter.Query != "" {
		values.Set("q", p.Filter.Query)
	}
	if p.Filter.Undeclared {
		values.Set("undeclared", "1")
	}
	return template.URL(adminPath + "/packages?" + values.Encode())
}

// paginate applies the filter and cuts out the requested page.
func paginate(items []inventoryItem, filter inventoryFilter, page int) inventoryPage {
	out := inventoryPage{Page: page, Filter: filter, Files: len(items)}

	for _, item := range items {
		out.Bytes += item.Size
	}

	matched := make([]inventoryItem, 0, len(items))
	for _, item := range items {
		if filter.matches(item) {
			matched = append(matched, item)
		}
	}

	out.Total = len(matched)
	out.Pages = (out.Total + pageSize - 1) / pageSize
	if out.Pages == 0 {
		out.Pages = 1
	}

	// Clamp rather than 404: a page number out of range is a stale link, and
	// the useful answer is the nearest page that exists.
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

// declarePackage adds a name to [destination].packages, or removes it, and
// saves the config.
//
// Adding does not download anything and removing does not delete a file: the
// list is what -ci resolves and what -cl keeps, so this changes what the next
// run will do, not what is on disk now. The page says so.
func declarePackage(configPath, name string, add bool) error {
	config, err := loadConfigFile(configPath)
	if err != nil {
		return err
	}

	existing := config.Destination.Packages
	index := slices.Index(existing, name)

	switch {
	case add && index >= 0:
		return nil // already there, nothing to write
	case add:
		config.Destination.Packages = append(slices.Clone(existing), name)
		// Sorted, so the list stays readable however it was added to.
		slices.Sort(config.Destination.Packages)
	case index < 0:
		return nil // not there, nothing to write
	default:
		config.Destination.Packages = slices.Delete(slices.Clone(existing), index, index+1)
	}

	return saveConfigFile(configPath, config)
}

// repoRoot is the destination path, resolved once per request from the saved
// config so the page follows a path the panel itself just changed.
func repoRoot(configPath string) (string, []string, error) {
	config, err := loadConfigFile(configPath)
	if err != nil {
		return "", nil, err
	}

	root := config.Destination.Path
	if _, err := os.Stat(root); err != nil {
		return "", nil, err
	}
	return root, config.Destination.Packages, nil
}
