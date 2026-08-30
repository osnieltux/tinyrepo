package tinyrepo

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// -vp answers one question about every name in [destination].packages: does the
// mirror actually have it?
//
// The index is the mirror's own answer to that. A name that is not in it is one
// -ci will report as unresolved and quietly leave out of the repository, which
// is how a typo becomes a repository that is silently missing a package.

// packageState is what became of one requested name.
type packageState string

const (
	// packageFound: a package by exactly that name exists.
	packageFound packageState = "found"
	// packageProvided: no package by that name, but something provides it, so
	// asking for it still works.
	packageProvided packageState = "provided"
	// packageGroup: an Arch group, which expands to its members.
	packageGroup packageState = "group"
	// packageMissing: nothing in the index answers to it. This is the one that
	// matters.
	packageMissing packageState = "missing"
)

// packageStatus is one requested name, for one architecture.
type packageStatus struct {
	Arch string
	Name string
	// State is what the index says about it.
	State packageState
	// Detail names the provider, or the members of a group. Empty otherwise.
	Detail string
}

// OK reports whether this name is one the mirror can actually deliver.
func (s packageStatus) OK() bool { return s.State != packageMissing }

// Describe renders the state in the reader's language, for both the terminal
// and the panel.
func (s packageStatus) Describe() string {
	switch s.State {
	case packageFound:
		return _t("pkg found")
	case packageProvided:
		return _t("pkg provided") + " " + s.Detail
	case packageGroup:
		return _t("pkg group") + " " + s.Detail
	}
	return _t("pkg missing")
}

// verifyResult is everything -vp and the panel need about one run.
type verifyResult struct {
	Statuses []packageStatus
	// Missing is how many names nothing in the index answers to, across every
	// architecture. It is what decides the exit code.
	Missing int
}

// nameCatalogs builds the resolver's view of the mirror, one catalog per
// configured architecture.
//
// A function rather than a method on Backend: the interface is about building
// and serving a repository, and adding to it would break every fake that
// implements it for a test.
func nameCatalogs(config *Config) (map[string]*catalog, error) {
	catalogs := map[string]*catalog{}

	switch config.Server.Type {
	case BackendArch:
		for _, arch := range config.Destination.Arch {
			// Without the files database: it is ten times the download and
			// says nothing about whether a package exists.
			index, err := loadArchIndexOpt(config, arch, false)
			if err != nil {
				return nil, err
			}
			catalogs[arch] = index.catalog()
		}

	default:
		byArch, err := loadDebIndex(config)
		if err != nil {
			return nil, err
		}
		for _, arch := range config.Destination.Arch {
			index := byArch[arch]
			if len(index) == 0 {
				logError(_t("arch no p"), arch)
				continue
			}
			catalogs[arch] = debCatalog(index)
		}
	}

	if len(catalogs) == 0 {
		return nil, fmt.Errorf("%s", _t("err r indexes"))
	}
	return catalogs, nil
}

// checkName is what the index says about one requested name.
func checkName(cat *catalog, name string) packageStatus {
	status := packageStatus{Name: name}

	if _, ok := cat.Deps[name]; ok {
		status.State = packageFound
		return status
	}

	// Groups first among the rest: an Arch group and a virtual name are both
	// "not a package", but only one of them expands.
	if members, ok := cat.Groups[name]; ok {
		status.State = packageGroup
		status.Detail = fmt.Sprintf("(%d)", len(members))
		return status
	}

	if providers := cat.Provides[name]; len(providers) > 0 {
		sorted := slices.Clone(providers)
		slices.Sort(sorted)

		status.State = packageProvided
		status.Detail = sorted[0]
		if len(sorted) > 1 {
			status.Detail += fmt.Sprintf(" (+%d)", len(sorted)-1)
		}
		return status
	}

	status.State = packageMissing
	return status
}

// verifyPackages checks every name in [destination].packages against the cached
// index, for every configured architecture.
func verifyPackages(config *Config) (*verifyResult, error) {
	if len(config.Destination.Packages) == 0 {
		return &verifyResult{}, nil
	}

	catalogs, err := nameCatalogs(config)
	if err != nil {
		return nil, err
	}

	result := &verifyResult{}

	for _, arch := range config.Destination.Arch {
		cat, ok := catalogs[arch]
		if !ok {
			continue
		}

		for _, name := range config.Destination.Packages {
			status := checkName(cat, name)
			status.Arch = arch

			if !status.OK() {
				result.Missing++
			}
			result.Statuses = append(result.Statuses, status)
		}
	}

	sortVerifyStatuses(result)
	return result, nil
}

// sortVerifyStatuses puts the missing entries first, then orders by
// architecture and name: the answer to "what is wrong" should not need
// scrolling past everything that is fine.
func sortVerifyStatuses(result *verifyResult) {
	sort.SliceStable(result.Statuses, func(i, j int) bool {
		a, b := result.Statuses[i], result.Statuses[j]
		if a.OK() != b.OK() {
			return !a.OK()
		}
		if a.Arch != b.Arch {
			return a.Arch < b.Arch
		}
		return a.Name < b.Name
	})
}

// showPackageCheck implements -vp.
func showPackageCheck() int {
	configPath, err := getConfigFile()
	if err != nil {
		logErrorf("%s: %v", _t("error f exec path"), err)
		return 9
	}
	if configPath == "" {
		logError(_t("err config no f"))
		return 1
	}

	config, err := loadConfigFile(configPath)
	if err != nil {
		logError(err)
		return 2
	}

	if len(config.Destination.Packages) == 0 {
		logError(_t("err no packages listed"))
		return 4
	}

	result, err := verifyPackages(config)
	if err != nil {
		// The index has to be there to check anything against: -di is what
		// puts it there.
		logErrorf("%s: %v", _t("err r indexes"), err)
		return 3
	}

	// To stdout, so the report can be piped somewhere, while the summary and
	// any error go to stderr like every other message.
	width := 0
	for _, status := range result.Statuses {
		if n := len(status.Name); n > width {
			width = n
		}
	}

	arch := ""
	for _, status := range result.Statuses {
		if status.Arch != arch {
			arch = status.Arch
			fmt.Printf("\n%s\n", arch)
		}

		mark := "ok "
		if !status.OK() {
			mark = "MISSING"
		}
		fmt.Printf("  %-7s %-*s  %s\n", mark, width, status.Name, status.Describe())
	}
	fmt.Println()

	if result.Missing > 0 {
		logErrorf("%s: %d", _t("packages missing"), result.Missing)
		return 18
	}

	logErrorf("%s: %d", _t("packages ok"), len(config.Destination.Packages))
	return 0
}

// missingNames lists the names nothing answers to, deduplicated across
// architectures, for a one line summary.
func (r *verifyResult) missingNames() []string {
	seen := map[string]bool{}
	var names []string

	for _, status := range r.Statuses {
		if status.OK() || seen[status.Name] {
			continue
		}
		seen[status.Name] = true
		names = append(names, status.Name)
	}

	slices.Sort(names)
	return names
}

// Summary is the single line the panel shows above the table.
func (r *verifyResult) Summary() string {
	if len(r.Statuses) == 0 {
		return _t("err no packages listed")
	}
	if r.Missing == 0 {
		return _t("packages ok")
	}
	return _t("packages missing") + ": " + strings.Join(r.missingNames(), ", ")
}
