package tinyrepo

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// checkName is the whole judgement: a name is either something the mirror can
// deliver, or a typo.

func verifyTestCatalog() *catalog {
	cat := newCatalog()
	cat.addPackage("nano", nil, nil, nil)
	cat.addPackage("wget", [][]string{{"libc6"}}, nil, nil)
	cat.addPackage("libc6", nil, nil, nil)
	cat.addPackage("exim4-daemon-light", nil, []string{"mail-transport-agent"}, nil)
	cat.addPackage("postfix", nil, []string{"mail-transport-agent"}, nil)
	cat.addPackage("kwrite", nil, nil, []string{"kde-applications"})
	cat.addPackage("konsole", nil, nil, []string{"kde-applications"})
	return cat
}

func TestCheckNameFindsARealPackage(t *testing.T) {
	status := checkName(verifyTestCatalog(), "nano")

	if status.State != packageFound {
		t.Errorf("state = %q, want %q", status.State, packageFound)
	}
	if !status.OK() {
		t.Error("a package that exists was reported as missing")
	}
}

// The case the flag exists for.
func TestCheckNameReportsATypo(t *testing.T) {
	status := checkName(verifyTestCatalog(), "nanoo")

	if status.State != packageMissing {
		t.Errorf("state = %q, want %q", status.State, packageMissing)
	}
	if status.OK() {
		t.Error("a name nothing answers to was reported as fine")
	}
}

// A virtual name is not a package, but asking for it still works, so it must
// not be reported as missing.
func TestCheckNameResolvesAVirtualName(t *testing.T) {
	status := checkName(verifyTestCatalog(), "mail-transport-agent")

	if status.State != packageProvided {
		t.Fatalf("state = %q, want %q", status.State, packageProvided)
	}
	if !status.OK() {
		t.Error("a provided name was reported as missing")
	}
	// The provider is named, and the count says the choice was not the only
	// one, because which provider gets picked is worth knowing.
	if !strings.Contains(status.Detail, "exim4-daemon-light") {
		t.Errorf("detail = %q, want the first provider named", status.Detail)
	}
	if !strings.Contains(status.Detail, "+1") {
		t.Errorf("detail = %q, want the other provider counted", status.Detail)
	}
}

// Two providers must not produce a different answer from run to run.
func TestCheckNameIsDeterministicAcrossProviders(t *testing.T) {
	first := checkName(verifyTestCatalog(), "mail-transport-agent").Detail

	for range 20 {
		if got := checkName(verifyTestCatalog(), "mail-transport-agent").Detail; got != first {
			t.Fatalf("detail changed between runs: %q then %q", first, got)
		}
	}
}

func TestCheckNameRecognisesAGroup(t *testing.T) {
	status := checkName(verifyTestCatalog(), "kde-applications")

	if status.State != packageGroup {
		t.Fatalf("state = %q, want %q", status.State, packageGroup)
	}
	if !strings.Contains(status.Detail, "2") {
		t.Errorf("detail = %q, want the member count", status.Detail)
	}
}

// A real package that also happens to be a group member is a package.
func TestCheckNamePrefersThePackageOverTheGroup(t *testing.T) {
	cat := newCatalog()
	cat.addPackage("konsole", nil, nil, []string{"konsole"})

	if status := checkName(cat, "konsole"); status.State != packageFound {
		t.Errorf("state = %q, want %q", status.State, packageFound)
	}
}

// ── the whole check ─────────────────────────────────────────────────────────

func verifyTestConfig(packages ...string) *Config {
	config := defaultConfig()
	config.Server.Source = []string{"https://deb.debian.org/debian bookworm main"}
	config.Destination.Path = "/tmp/repo"
	config.Destination.Arch = []string{"amd64"}
	config.Destination.Packages = packages
	return &config
}

// verifyWith runs verifyPackages against a catalog built by hand, which is what
// lets the reporting be tested without a cached index on disk.
func verifyWith(config *Config, catalogs map[string]*catalog) *verifyResult {
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
	return result
}

func TestVerifyCountsWhatIsMissing(t *testing.T) {
	config := verifyTestConfig("nano", "nanoo", "wget", "not-a-package")
	result := verifyWith(config, map[string]*catalog{"amd64": verifyTestCatalog()})

	if result.Missing != 2 {
		t.Errorf("missing = %d, want 2", result.Missing)
	}

	got := result.missingNames()
	want := []string{"nanoo", "not-a-package"}
	if len(got) != len(want) {
		t.Fatalf("missingNames = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("missingNames = %q, want %q", got, want)
			break
		}
	}
}

// A name missing on one architecture and present on another is still a problem,
// and has to be reported against the architecture it is missing from.
func TestVerifyChecksEveryArchitecture(t *testing.T) {
	amd64 := verifyTestCatalog()
	i386 := newCatalog()
	i386.addPackage("nano", nil, nil, nil) // no wget here

	config := verifyTestConfig("nano", "wget")
	config.Destination.Arch = []string{"amd64", "i386"}

	result := verifyWith(config, map[string]*catalog{"amd64": amd64, "i386": i386})

	if result.Missing != 1 {
		t.Fatalf("missing = %d, want 1", result.Missing)
	}
	for _, status := range result.Statuses {
		if status.Name == "wget" && status.Arch == "i386" && status.OK() {
			t.Error("wget was reported as present on i386, where it is not")
		}
	}
}

// The missing entries come first: the answer to "what is wrong" should not be
// below a screen of things that are fine.
func TestVerifySortsMissingFirst(t *testing.T) {
	config := verifyTestConfig("nano", "zzz-missing", "wget")

	result := verifyWith(config, map[string]*catalog{"amd64": verifyTestCatalog()})
	// verifyWith does not sort; verifyPackages does, so sort the same way here
	// by running the exported path's comparison over the result.
	sorted := &verifyResult{Statuses: result.Statuses, Missing: result.Missing}
	sortVerifyStatuses(sorted)

	if sorted.Statuses[0].Name != "zzz-missing" {
		t.Errorf("first row is %q, want the missing package first", sorted.Statuses[0].Name)
	}
}

// An empty package list is not a failure, it is a config with nothing to check.
func TestVerifyWithNoPackagesIsNotAnError(t *testing.T) {
	result, err := verifyPackages(verifyTestConfig())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result.Missing != 0 || len(result.Statuses) != 0 {
		t.Error("an empty package list produced a report")
	}
}

// verifyPackages needs an index; without one it has to say so rather than
// report every package as missing.
func TestVerifyWithoutACachedIndexIsAnError(t *testing.T) {
	config := verifyTestConfig("nano")
	config.Destination.Path = t.TempDir()

	if _, err := verifyPackages(config); err == nil {
		t.Error("verifying with no cached index reported success")
	}
}

func TestVerifySummary(t *testing.T) {
	config := verifyTestConfig("nano", "nanoo")
	result := verifyWith(config, map[string]*catalog{"amd64": verifyTestCatalog()})

	if summary := result.Summary(); !strings.Contains(summary, "nanoo") {
		t.Errorf("summary = %q, want the missing name in it", summary)
	}

	clean := verifyWith(verifyTestConfig("nano"), map[string]*catalog{"amd64": verifyTestCatalog()})
	if summary := clean.Summary(); strings.Contains(summary, "nano") {
		t.Errorf("summary = %q, want no package names when nothing is missing", summary)
	}
}

// ── the panel ───────────────────────────────────────────────────────────────

func TestPanelVerifyNeedsTheCSRFToken(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	if response := f.post(adminPath+"/verify", url.Values{}); response.StatusCode != http.StatusForbidden {
		t.Errorf("got %d, want 403", response.StatusCode)
	}
}

func TestPanelVerifyNeedsASession(t *testing.T) {
	f := newAdminFixture(t)

	if response := f.post(adminPath+"/verify", url.Values{}); response.StatusCode != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", response.StatusCode)
	}
}

// Without a cached index the panel has to stay usable and say what is wrong,
// not fall over.
func TestPanelVerifyReportsAMissingIndex(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()

	response := f.post(adminPath+"/verify", url.Values{"csrf": {csrf}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", response.StatusCode)
	}

	body := f.body(response)
	if !strings.Contains(body, "csrf") {
		t.Error("the dashboard did not come back")
	}
}
