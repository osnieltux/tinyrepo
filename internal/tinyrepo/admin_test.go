package tinyrepo

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// adminFixture is a panel wired to a real config.toml in a temporary directory,
// served over a test server so the cookie and redirect behaviour is the real
// one rather than a handler called by hand.
type adminFixture struct {
	t          *testing.T
	server     *httptest.Server
	client     *http.Client
	configPath string
	panel      *adminPanel
}

const adminTestPassword = "correct horse battery"

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()

	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}

	hash, err := hashPassword(adminTestPassword)
	if err != nil {
		t.Fatal(err)
	}

	config := defaultConfig()
	config.Server.Source = []string{"https://deb.debian.org/debian bookworm main"}
	config.Destination.Path = repo
	config.Destination.Packages = []string{"nano"}
	config.Web.Config = true
	config.Web.User = "admin"
	config.Web.PasswordHash = hash

	configPath := filepath.Join(dir, "config.toml")
	if err := saveConfigFile(configPath, &config); err != nil {
		t.Fatal(err)
	}

	panel, err := newAdminPanel(&config, configPath)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(newHandler(&config, nil, newServerStats(), panel))
	t.Cleanup(server.Close)

	jar := &cookieJar{}
	return &adminFixture{
		t:          t,
		server:     server,
		client:     &http.Client{Jar: jar, CheckRedirect: noRedirect},
		configPath: configPath,
		panel:      panel,
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// cookieJar is the smallest jar that keeps one host's cookies, which is all a
// single test server needs.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	for _, c := range cookies {
		if c.MaxAge < 0 {
			j.cookies = nil
			continue
		}
		j.cookies = append(j.cookies, c)
	}
}
func (j *cookieJar) Cookies(*url.URL) []*http.Cookie { return j.cookies }

func (f *adminFixture) get(path string) *http.Response {
	f.t.Helper()

	response, err := f.client.Get(f.server.URL + path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { response.Body.Close() })
	return response
}

func (f *adminFixture) post(path string, form url.Values) *http.Response {
	f.t.Helper()

	response, err := f.client.PostForm(f.server.URL+path, form)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { response.Body.Close() })
	return response
}

func (f *adminFixture) body(response *http.Response) string {
	f.t.Helper()

	buf := make([]byte, 256<<10)
	n, _ := response.Body.Read(buf)
	return string(buf[:n])
}

// login signs in and returns the session's CSRF token, read back out of the
// dashboard exactly as a browser would submit it.
func (f *adminFixture) login() string {
	f.t.Helper()

	response := f.post(adminPath+"/login", url.Values{
		"user":     {"admin"},
		"password": {adminTestPassword},
	})
	if response.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("login: got %d, want 303", response.StatusCode)
	}
	return f.csrf()
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (f *adminFixture) csrf() string {
	f.t.Helper()

	match := csrfPattern.FindStringSubmatch(f.body(f.get(adminPath + "/")))
	if len(match) != 2 {
		f.t.Fatal("no csrf token on the dashboard")
	}
	return match[1]
}

func (f *adminFixture) savedConfig() *Config {
	f.t.Helper()

	config, err := loadConfigFile(f.configPath)
	if err != nil {
		f.t.Fatal(err)
	}
	return config
}

// ── password hashing ────────────────────────────────────────────────────────

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := hashPassword("hunter2 hunter2")
	if err != nil {
		t.Fatal(err)
	}

	ok, err := verifyPassword(hash, "hunter2 hunter2")
	if err != nil || !ok {
		t.Errorf("the right password was rejected: %v", err)
	}

	ok, err = verifyPassword(hash, "hunter2 hunter3")
	if err != nil || ok {
		t.Error("the wrong password was accepted")
	}
}

// The salt is what stops one leaked hash from breaking every account that
// happened to choose the same password.
func TestPasswordHashIsSalted(t *testing.T) {
	first, _ := hashPassword("the same password")
	second, _ := hashPassword("the same password")

	if first == second {
		t.Error("two hashes of one password are identical, the salt is not being used")
	}
}

func TestPasswordHashCarriesItsParameters(t *testing.T) {
	hash, _ := hashPassword("a password long enough")

	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		t.Fatalf("unexpected hash format: %q", hash)
	}
	// The stored iteration count is what lets the cost be raised later without
	// invalidating hashes produced today.
	if parts[1] != "600000" {
		t.Errorf("iterations = %q, want 600000", parts[1])
	}
	if strings.Contains(hash, "a password long enough") {
		t.Error("the hash contains the password")
	}
}

func TestVerifyPasswordRejectsAMalformedHash(t *testing.T) {
	for _, stored := range []string{
		"", "plaintext", "pbkdf2-sha256$notanumber$c2FsdA$a2V5",
		"pbkdf2-sha256$1000$!!!$a2V5", "scrypt$1000$c2FsdA$a2V5",
		"pbkdf2-sha256$1000$c2FsdA", "pbkdf2-sha256$0$c2FsdA$a2V5",
	} {
		if _, err := verifyPassword(stored, "whatever"); err == nil {
			t.Errorf("%q was accepted as a hash", stored)
		}
	}
}

// A panel that is on but has no usable login must be caught when the config is
// read, not when somebody finally tries the login page.
func TestValidateConfigRejectsPanelWithoutCredentials(t *testing.T) {
	base := func() Config {
		cfg := defaultConfig()
		cfg.Server.Source = []string{"https://example.invalid/debian bookworm main"}
		cfg.Destination.Path = "/tmp/repo"
		cfg.Web.Config = true
		return cfg
	}

	tests := map[string]func(*Config){
		"no user and no hash": func(*Config) {},
		"user but no hash":    func(c *Config) { c.Web.User = "admin" },
		"hash but no user":    func(c *Config) { c.Web.PasswordHash = "pbkdf2-sha256$1$c2FsdA$a2V5" },
		"unusable hash":       func(c *Config) { c.Web.User = "admin"; c.Web.PasswordHash = "plaintext" },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(&cfg)
			if err := validateConfig(&cfg); err == nil {
				t.Error("the config was accepted with an unusable panel login")
			}
		})
	}

	t.Run("valid", func(t *testing.T) {
		cfg := base()
		cfg.Web.User = "admin"
		cfg.Web.PasswordHash, _ = hashPassword(adminTestPassword)
		if err := validateConfig(&cfg); err != nil {
			t.Errorf("a usable panel login was rejected: %v", err)
		}
	})
}

// ── access control ──────────────────────────────────────────────────────────

// The panel must not exist at all unless it was asked for, or every -ws would
// publish a login page to whoever can reach the repository.
func TestPanelIsNotPublishedUnlessEnabled(t *testing.T) {
	dir := t.TempDir()
	config := defaultConfig()
	config.Destination.Path = dir

	server := httptest.NewServer(newHandler(&config, nil, newServerStats(), nil))
	defer server.Close()

	for _, path := range []string{adminPath, adminPath + "/", adminPath + "/login"} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()

		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404 with the panel off", path, response.StatusCode)
		}
	}
}

func TestPanelRedirectsToLoginWhenSignedOut(t *testing.T) {
	f := newAdminFixture(t)

	response := f.get(adminPath + "/")
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", response.StatusCode)
	}
	if location := response.Header.Get("Location"); location != adminPath+"/login" {
		t.Errorf("Location = %q, want the login page", location)
	}
}

// Every writing endpoint has to check the session, not just the dashboard: an
// unauthenticated POST to /save or /run would otherwise be the whole panel.
func TestPanelWritingEndpointsRefuseWithoutASession(t *testing.T) {
	f := newAdminFixture(t)

	for _, path := range []string{"/save", "/run", "/logout"} {
		response := f.post(adminPath+path, url.Values{"job": {"di"}})
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST %s: got %d, want 401", path, response.StatusCode)
		}
	}

	// And nothing was run or written.
	if f.panel.jobs.snapshot() != nil {
		t.Error("an unauthenticated POST started an action")
	}
}

func TestPanelLoginRejectsTheWrongPassword(t *testing.T) {
	f := newAdminFixture(t)

	response := f.post(adminPath+"/login", url.Values{
		"user":     {"admin"},
		"password": {"not it"},
	})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", response.StatusCode)
	}

	// Still signed out.
	if response := f.get(adminPath + "/"); response.StatusCode != http.StatusSeeOther {
		t.Error("a failed login produced a session")
	}
}

// The same answer for a wrong user and a wrong password: a different one tells
// a guesser which half to keep.
func TestPanelLoginDoesNotRevealWhichHalfWasWrong(t *testing.T) {
	f := newAdminFixture(t)

	wrongUser := f.body(f.post(adminPath+"/login", url.Values{
		"user": {"nobody"}, "password": {adminTestPassword}}))
	wrongPassword := f.body(f.post(adminPath+"/login", url.Values{
		"user": {"admin"}, "password": {"not it"}}))

	if wrongUser != wrongPassword {
		t.Error("the login page says which half of the credentials was wrong")
	}
}

func TestPanelThrottlesRepeatedFailures(t *testing.T) {
	f := newAdminFixture(t)

	var last *http.Response
	for range loginBurst + 1 {
		last = f.post(adminPath+"/login", url.Values{
			"user": {"admin"}, "password": {"not it"}})
	}

	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d after %d failures, want 429", last.StatusCode, loginBurst+1)
	}

	// And the brake holds even once the right password shows up, so guessing
	// cannot simply be retried until it works.
	response := f.post(adminPath+"/login", url.Values{
		"user": {"admin"}, "password": {adminTestPassword}})
	if response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("got %d, want the throttle to still apply", response.StatusCode)
	}
}

func TestPanelSessionCookieIsHardened(t *testing.T) {
	f := newAdminFixture(t)

	response := f.post(adminPath+"/login", url.Values{
		"user": {"admin"}, "password": {adminTestPassword}})

	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}

	cookie := cookies[0]
	if !cookie.HttpOnly {
		t.Error("the session cookie is readable from JavaScript")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie is not SameSite=Strict")
	}
	if cookie.Path != "/" {
		t.Errorf("cookie path = %q, want /", cookie.Path)
	}
	if strings.Contains(cookie.Value, adminTestPassword) || cookie.Value == "admin" {
		t.Error("the session cookie carries the credentials rather than a token")
	}
}

func TestPanelLogoutEndsTheSession(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()

	response := f.post(adminPath+"/logout", url.Values{"csrf": {csrf}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: got %d, want 303", response.StatusCode)
	}

	if response := f.get(adminPath + "/"); response.StatusCode != http.StatusSeeOther {
		t.Error("the session survived a logout")
	}
}

func TestSessionExpiresWhenIdle(t *testing.T) {
	store := newSessionStore()

	id, sess, err := store.create("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.get(id); !ok {
		t.Fatal("a fresh session was not found")
	}

	sess.seen = time.Now().Add(-sessionIdle - time.Minute)
	if _, ok := store.get(id); ok {
		t.Error("an idle session is still live")
	}
}

// An active session still has to end: a stolen cookie must not be good forever.
func TestSessionExpiresAtItsMaximumAge(t *testing.T) {
	store := newSessionStore()

	id, sess, _ := store.create("admin")
	sess.created = time.Now().Add(-sessionMax - time.Minute)
	sess.seen = time.Now()

	if _, ok := store.get(id); ok {
		t.Error("a session past its maximum age is still live")
	}
}

// ── CSRF ────────────────────────────────────────────────────────────────────

// Without this, any page the user visits while signed in can reconfigure the
// repository or start a download by posting a form at it.
func TestPanelRefusesAWriteWithoutTheCSRFToken(t *testing.T) {
	f := newAdminFixture(t)
	f.login()
	before := f.savedConfig()

	for _, test := range []struct {
		path string
		form url.Values
	}{
		{"/save", url.Values{"type": {"debian"}, "path": {"/tmp/hijacked"}}},
		{"/save", url.Values{"csrf": {"wrong"}, "path": {"/tmp/hijacked"}}},
		{"/run", url.Values{"job": {"di"}}},
		{"/logout", url.Values{}},
	} {
		response := f.post(adminPath+test.path, test.form)
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without a token: got %d, want 403", test.path, response.StatusCode)
		}
	}

	if after := f.savedConfig(); after.Destination.Path != before.Destination.Path {
		t.Error("a request without a CSRF token rewrote the configuration")
	}
	if f.panel.jobs.snapshot() != nil {
		t.Error("a request without a CSRF token started an action")
	}
}

// One session's token must not work for another's.
func TestCSRFTokensAreOnePerSession(t *testing.T) {
	store := newSessionStore()

	_, first, _ := store.create("admin")
	_, second, _ := store.create("admin")

	if first.csrf == second.csrf {
		t.Error("two sessions share a CSRF token")
	}
}

// ── saving ──────────────────────────────────────────────────────────────────

func TestPanelSavesTheForm(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()

	response := f.post(adminPath+"/save", url.Values{
		"csrf":                   {csrf},
		"type":                   {"debian"},
		"source":                 {"https://deb.debian.org/debian bookworm main contrib\n\n  https://security.debian.org/debian-security bookworm-security main  "},
		"arch":                   {"amd64\ni386"},
		"path":                   {"/tmp/newrepo"},
		"packages":               {"nano\nwget\ncurl"},
		"listen":                 {"0.0.0.0:9090"},
		"health":                 {"on"},
		"panel":                  {"on"},
		"user":                   {"operator"},
		"proxyHost":              {"http://127.0.0.1"},
		"proxyPort":              {"3128"},
		"maxConcurrentDownloads": {"8"},
		"verifyChecksum":         {"on"},
		"onDemand":               {"on"},
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: got %d, want 303\n%s", response.StatusCode, f.body(response))
	}

	saved := f.savedConfig()

	if got, want := len(saved.Server.Source), 2; got != want {
		t.Errorf("sources = %d, want %d - blank lines and spaces should be dropped", got, want)
	}
	if saved.Server.Source[1] != "https://security.debian.org/debian-security bookworm-security main" {
		t.Errorf("source was not trimmed: %q", saved.Server.Source[1])
	}
	if got := saved.Destination.Path; got != "/tmp/newrepo" {
		t.Errorf("path = %q", got)
	}
	if got, want := len(saved.Destination.Packages), 3; got != want {
		t.Errorf("packages = %d, want %d", got, want)
	}
	if got := saved.Web.Listen; got != "0.0.0.0:9090" {
		t.Errorf("listen = %q", got)
	}
	if got := saved.Settings.MaxConcurrentDownloads; got != 8 {
		t.Errorf("maxConcurrentDownloads = %d, want 8", got)
	}
	if !saved.Settings.OnDemand || !saved.Web.Health {
		t.Error("a checked box was not saved")
	}
	// An unchecked box sends nothing at all, which has to read as false rather
	// than as "leave it alone".
	if saved.Settings.SkipDownloadSameSize {
		t.Error("an unchecked box kept its previous true value")
	}
}

// An invalid submission must be refused whole: half a config written to disk is
// a repository that will not start.
func TestPanelRejectsAnInvalidFormWithoutWriting(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()
	before := f.savedConfig()

	tests := map[string]url.Values{
		"unknown backend":   {"type": {"gentoo"}, "listen": {"127.0.0.1:8080"}},
		"no source":         {"type": {"debian"}, "source": {""}, "listen": {"127.0.0.1:8080"}},
		"bad listen":        {"type": {"debian"}, "listen": {"not-a-host-port"}},
		"arch all":          {"type": {"debian"}, "arch": {"all"}, "listen": {"127.0.0.1:8080"}},
		"downloads too big": {"type": {"debian"}, "maxConcurrentDownloads": {"999"}, "listen": {"127.0.0.1:8080"}},
		"downloads not a number": {"type": {"debian"}, "maxConcurrentDownloads": {"lots"},
			"listen": {"127.0.0.1:8080"}},
	}

	for name, form := range tests {
		t.Run(name, func(t *testing.T) {
			form.Set("csrf", csrf)
			form.Set("path", "/tmp/repo")
			if form.Get("source") == "" && name != "no source" {
				form.Set("source", "https://deb.debian.org/debian bookworm main")
			}
			if form.Get("arch") == "" {
				form.Set("arch", "amd64")
			}
			if form.Get("maxConcurrentDownloads") == "" {
				form.Set("maxConcurrentDownloads", "4")
			}
			if form.Get("proxyPort") == "" {
				form.Set("proxyPort", "3128")
			}

			response := f.post(adminPath+"/save", form)
			if response.StatusCode != http.StatusBadRequest {
				t.Errorf("got %d, want 400", response.StatusCode)
			}
		})
	}

	if after := f.savedConfig(); after.Destination.Path != before.Destination.Path ||
		after.Server.Type != before.Server.Type {
		t.Error("a rejected submission still changed config.toml")
	}
}

// An empty password field means "keep the current one". Anything else would
// wipe the login every time the form is saved.
func TestPanelKeepsThePasswordWhenTheFieldIsBlank(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()
	before := f.savedConfig().Web.PasswordHash

	f.post(adminPath+"/save", url.Values{
		"csrf": {csrf}, "type": {"debian"},
		"source": {"https://deb.debian.org/debian bookworm main"},
		"arch":   {"amd64"}, "path": {"/tmp/repo"},
		"listen": {"127.0.0.1:8080"}, "panel": {"on"}, "user": {"admin"},
		"maxConcurrentDownloads": {"4"}, "proxyPort": {"3128"},
		"newPassword": {""},
	})

	if after := f.savedConfig().Web.PasswordHash; after != before {
		t.Error("saving the form with a blank password field changed the password")
	}
}

func TestPanelChangesThePassword(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()

	form := url.Values{
		"csrf": {csrf}, "type": {"debian"},
		"source": {"https://deb.debian.org/debian bookworm main"},
		"arch":   {"amd64"}, "path": {"/tmp/repo"},
		"listen": {"127.0.0.1:8080"}, "panel": {"on"}, "user": {"admin"},
		"maxConcurrentDownloads": {"4"}, "proxyPort": {"3128"},
	}

	// Too short is refused rather than quietly accepted.
	form.Set("newPassword", "short")
	if response := f.post(adminPath+"/save", form); response.StatusCode != http.StatusBadRequest {
		t.Errorf("a five character password: got %d, want 400", response.StatusCode)
	}

	form.Set("newPassword", "a brand new password")
	if response := f.post(adminPath+"/save", form); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", response.StatusCode)
	}

	saved := f.savedConfig()
	if ok, err := verifyPassword(saved.Web.PasswordHash, "a brand new password"); err != nil || !ok {
		t.Error("the new password does not verify against the saved hash")
	}
	if strings.Contains(saved.Web.PasswordHash, "a brand new password") {
		t.Error("the plaintext password reached config.toml")
	}
}

// The password must never be rendered back into the form, or a browser's
// autofill hands it to whatever reads the page.
func TestPanelNeverEchoesTheCredentials(t *testing.T) {
	f := newAdminFixture(t)
	f.login()

	page := f.body(f.get(adminPath + "/"))

	if strings.Contains(page, adminTestPassword) {
		t.Error("the dashboard contains the password")
	}
	if strings.Contains(page, f.savedConfig().Web.PasswordHash) {
		t.Error("the dashboard contains the password hash")
	}
}

// ── actions ─────────────────────────────────────────────────────────────────

func TestPanelRefusesAnUnknownAction(t *testing.T) {
	f := newAdminFixture(t)
	csrf := f.login()

	response := f.post(adminPath+"/run", url.Values{"csrf": {csrf}, "job": {"rm -rf"}})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d, want 400", response.StatusCode)
	}
	if f.panel.jobs.snapshot() != nil {
		t.Error("an unknown action was started")
	}
}

// The four actions all read and write the same directory, so only one may run.
func TestJobRunnerRunsOneAtATime(t *testing.T) {
	runner := newJobRunner("config.toml")

	release := make(chan struct{})
	runner.mu.Lock()
	runner.current = &jobRun{Kind: jobBuild, Started: time.Now(), Running: true}
	held := runner.current
	runner.mu.Unlock()

	if err := runner.start(jobFetchIndexes); err == nil {
		t.Error("a second action started while one was running")
	}
	if !runner.busy() {
		t.Error("the runner does not report itself busy")
	}

	runner.finish(held, nil)
	close(release)

	if runner.busy() {
		t.Error("the runner is still busy after the run finished")
	}
}

// finish is reached from both the normal path and the panic recovery, so it has
// to be safe to call twice - the first result is the one that counts.
func TestJobFinishIsIdempotent(t *testing.T) {
	runner := newJobRunner("config.toml")
	run := &jobRun{Kind: jobBuild, Started: time.Now(), Running: true}

	runner.mu.Lock()
	runner.current = run
	runner.mu.Unlock()

	runner.finish(run, nil)
	runner.finish(run, errJobBusy)

	if got := runner.snapshot(); got.Err != nil {
		t.Errorf("a second finish overwrote the result: %v", got.Err)
	}
}

// A -dp over a large suite prints a line per package; the panel is not a log
// server and must not grow without bound.
func TestJobLogIsBounded(t *testing.T) {
	runner := newJobRunner("config.toml")
	run := &jobRun{Kind: jobDownloadPool, Started: time.Now(), Running: true}

	for i := range maxJobLogLines + 50 {
		runner.appendLog(run, "line "+strconv.Itoa(i))
	}

	if len(run.Log) != maxJobLogLines {
		t.Errorf("log holds %d lines, want it capped at %d", len(run.Log), maxJobLogLines)
	}
	// The cap drops the oldest, so the end of a run is what survives.
	if !strings.Contains(run.Log[len(run.Log)-1], "line "+strconv.Itoa(maxJobLogLines+49)) {
		t.Error("the most recent line was dropped instead of the oldest")
	}
}

// A run reads config.toml again rather than using what -ws started with, so an
// action always uses what was just saved from the panel.
func TestJobUsesTheSavedConfig(t *testing.T) {
	f := newAdminFixture(t)

	runner := newJobRunner(f.configPath)
	run := &jobRun{Kind: jobClean, Started: time.Now(), Running: true}
	runner.mu.Lock()
	runner.current = run
	runner.mu.Unlock()

	// A config that cannot be loaded has to fail the run, not the server.
	if err := os.WriteFile(f.configPath, []byte("this is not toml {{"), 0600); err != nil {
		t.Fatal(err)
	}

	runner.run(run)

	got := runner.snapshot()
	if got.Err == nil {
		t.Error("a broken config.toml produced a successful run")
	}
	if got.Running {
		t.Error("the run never finished")
	}
}

// ── routing ─────────────────────────────────────────────────────────────────

// The panel is answered before the filesystem, so a directory that happens to
// be called "admin" cannot shadow it - and a file called "administration" must
// still be served.
func TestAdminPathMatchesOnlyThePanel(t *testing.T) {
	tests := map[string]bool{
		"/admin":              true,
		"/admin/":             true,
		"/admin/login":        true,
		"/admin/run":          true,
		"/administration":     false,
		"/pool/admin":         false,
		"/adminstuff":         false,
		"/":                   false,
		"/dists/admin/README": false,
	}

	for path, want := range tests {
		if got := isAdminPath(path); got != want {
			t.Errorf("isAdminPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// The panel accepts POST; the repository must not.
func TestRepositoryStillRefusesPost(t *testing.T) {
	f := newAdminFixture(t)

	response := f.post("/pool/", url.Values{})
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to the repository: got %d, want 405", response.StatusCode)
	}
}

func TestPanelSetsItsHeaders(t *testing.T) {
	f := newAdminFixture(t)
	response := f.get(adminPath + "/login")

	want := map[string]string{
		"Cache-Control":          "no-store, private",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, value := range want {
		if got := response.Header.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}

	csp := response.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	// The panel has no script of its own, so nothing should permit one.
	if strings.Contains(csp, "script-src") {
		t.Errorf("the policy allows scripts: %q", csp)
	}
}

// ── loopback detection ──────────────────────────────────────────────────────

func TestIsLoopback(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		"127.0.0.53:80":  true,
		"0.0.0.0:8080":   false,
		"[::]:8080":      false,
		":8080":          false,
		"192.168.1.5:80": false,
		"nonsense":       false,
	}

	for listen, want := range tests {
		if got := isLoopback(listen); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", listen, got, want)
		}
	}
}

// The login brake counts against the real peer unless a proxy is configured:
// counting a header anyone can set would let a guesser both dodge the brake and
// lock somebody else out of it.
func TestClientAddrIgnoresForwardedHeaderWithoutAProxy(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, adminPath+"/login", nil)
	request.RemoteAddr = "203.0.113.9:5555"
	request.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")

	if got := clientAddr(request, false); got != "203.0.113.9" {
		t.Errorf("clientAddr = %q, want the peer address", got)
	}
	if got := clientAddr(request, true); got != "10.0.0.1" {
		t.Errorf("behind a proxy: clientAddr = %q, want the first forwarded entry", got)
	}
}

// ── config file round trip ──────────────────────────────────────────────────

// What the panel writes has to be what the command line reads back, or saving
// from the browser quietly breaks the next run.
func TestSavedConfigRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	original := defaultConfig()
	original.Server.Type = BackendArch
	original.Server.Source = []string{
		"https://geo.mirror.pkgbuild.com/$repo/os/$arch core extra",
		"https://other.example/$repo/os/$arch multilib",
	}
	original.Destination.Arch = []string{"x86_64"}
	original.Destination.Path = `/tmp/repo with spaces/`
	original.Destination.Packages = []string{"nano", "wget"}
	original.Web.Listen = "0.0.0.0:9090"
	original.Web.Config = true
	original.Web.User = "admin"
	original.Web.PasswordHash, _ = hashPassword(adminTestPassword)
	original.Proxy.Use = true
	original.Proxy.Host = "http://127.0.0.1"
	original.Proxy.Port = 3128
	original.Settings.OnDemand = true
	original.Settings.MaxConcurrentDownloads = 12

	if err := saveConfigFile(path, &original); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(original, *loaded) {
		t.Errorf("the config did not survive a round trip\nwrote %+v\nread  %+v", original, *loaded)
	}
}

// A Windows path is full of backslashes, which a naively written TOML string
// would turn into escape sequences.
func TestSavedConfigEscapesBackslashes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	config := defaultConfig()
	config.Server.Source = []string{"https://deb.debian.org/debian bookworm main"}
	config.Destination.Path = `C:\repo\Downloads`

	if err := saveConfigFile(path, &config); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Destination.Path != `C:\repo\Downloads` {
		t.Errorf("path = %q, want the backslashes intact", loaded.Destination.Path)
	}
}

// saveConfigFile validates before it writes, so the panel cannot leave behind a
// file the next run would refuse to start with.
func TestSaveConfigFileRefusesAnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	config := defaultConfig()
	config.Server.Source = nil // mandatory

	if err := saveConfigFile(path, &config); err == nil {
		t.Error("an invalid config was written")
	}
	if fileExists(path) {
		t.Error("a rejected save still created the file")
	}
}

func TestParseLines(t *testing.T) {
	got := parseLines("  nano \n\n\twget\t\n   \ncurl")

	want := []string{"nano", "wget", "curl"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLines = %q, want %q", got, want)
	}
}
