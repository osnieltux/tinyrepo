package tinyrepo

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// adminPath is where the panel lives. Everything under it is answered before
// the filesystem is consulted, and hidden from directory listings, so a
// repository that happens to contain an "admin" directory cannot shadow it and
// the panel cannot be mistaken for part of the published repository.
const adminPath = "/admin"

// adminPanel is the configuration panel: the login, the form that writes
// config.toml, and the buttons that run the actions.
type adminPanel struct {
	// configPath is the file the panel reads and writes. Fixed at startup, so
	// nothing a request says can redirect the write somewhere else.
	configPath string

	user string
	hash string

	sessions *sessionStore
	jobs     *jobRunner
	// catalog keeps the parsed upstream index between requests: a Debian suite
	// is ~50 MB of stanzas and re-reading it per page view would make the
	// listing unusable.
	catalog availableCache

	behindProxy bool
	// secure says the cookie may carry the Secure attribute and the __Host-
	// prefix, which requires HTTPS. Set when the connection is TLS or a proxy
	// in front terminates it.
	secure bool
}

func newAdminPanel(config *Config, configPath string) (*adminPanel, error) {
	user, hash, err := authCredentials(config)
	if err != nil {
		return nil, err
	}

	return &adminPanel{
		configPath:  configPath,
		user:        user,
		hash:        hash,
		sessions:    newSessionStore(),
		jobs:        newJobRunner(configPath),
		behindProxy: config.Web.BehindProxy,
	}, nil
}

// cookieName picks the hardened cookie name when the connection can carry it.
func (a *adminPanel) cookieName(r *http.Request) string {
	if a.isSecure(r) {
		return sessionSecureCookie
	}
	return sessionCookie
}

func (a *adminPanel) isSecure(r *http.Request) bool {
	return requestScheme(r, a.behindProxy) == "https"
}

// serve routes every request under adminPath.
func (a *adminPanel) serve(w http.ResponseWriter, r *http.Request) {
	// The panel is never cached: a logged-in page left in a shared cache, or in
	// the back button after logging out, is the whole configuration.
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	// The panel serves its own inline stylesheet and nothing else at all: no
	// script, no image, no font, and it may not be framed.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")

	action := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, adminPath), "/")

	if action == "login" {
		a.serveLogin(w, r)
		return
	}

	sess, id, ok := a.currentSession(r)
	if !ok {
		// A POST arriving without a session is not redirected: 303 to the login
		// page would silently drop whatever was being submitted.
		if r.Method == http.MethodPost {
			a.render(w, r, http.StatusUnauthorized, "login", loginPage{Error: _t("err session expired")})
			return
		}
		http.Redirect(w, r, adminPath+"/login", http.StatusSeeOther)
		return
	}

	switch action {
	case "", "/":
		a.serveDashboard(w, r, sess, "")
	case "save":
		a.serveSave(w, r, sess)
	case "run":
		a.serveRun(w, r, sess)
	case "verify":
		a.serveVerify(w, r, sess)
	case "packages":
		a.servePackages(w, r, sess)
	case "available":
		a.serveAvailable(w, r, sess)
	case "log":
		a.serveLog(w, r, sess)
	case "logout":
		a.serveLogout(w, r, sess, id)
	default:
		httpError(w, r, http.StatusNotFound)
	}
}

// currentSession returns the session a request carries, if it is still live.
func (a *adminPanel) currentSession(r *http.Request) (*session, string, bool) {
	cookie, err := r.Cookie(a.cookieName(r))
	if err != nil {
		// Falling back to the plain name covers a session opened before a proxy
		// started terminating TLS, so the change does not look like a bug.
		cookie, err = r.Cookie(sessionCookie)
		if err != nil {
			return nil, "", false
		}
	}

	sess, ok := a.sessions.get(cookie.Value)
	if !ok {
		return nil, "", false
	}
	return sess, cookie.Value, true
}

// checkCSRF verifies that a state-changing request came from the panel itself.
func (a *adminPanel) checkCSRF(r *http.Request, sess *session) bool {
	if r.Method != http.MethodPost {
		return false
	}
	got := r.PostFormValue("csrf")
	return subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) == 1
}

// ── login ───────────────────────────────────────────────────────────────────

func (a *adminPanel) serveLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if _, _, ok := a.currentSession(r); ok {
			http.Redirect(w, r, adminPath+"/", http.StatusSeeOther)
			return
		}
		a.render(w, r, http.StatusOK, "login", loginPage{Insecure: !a.isSecure(r)})
		return
	}

	if r.Method != http.MethodPost {
		httpError(w, r, http.StatusMethodNotAllowed)
		return
	}

	addr := clientAddr(r, a.behindProxy)
	if a.sessions.throttled(addr) {
		logErrorf("%s: %s", _t("err login throttled"), addr)
		w.Header().Set("Retry-After", strconv.Itoa(int(loginWindow.Seconds())))
		a.render(w, r, http.StatusTooManyRequests, "login",
			loginPage{Error: _t("err login throttled"), Insecure: !a.isSecure(r)})
		return
	}

	// A bounded body: a login form is a few hundred bytes, and nothing here
	// should let an unauthenticated client hand us a gigabyte to parse.
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		httpError(w, r, http.StatusBadRequest)
		return
	}

	user := r.PostFormValue("user")
	password := r.PostFormValue("password")

	// Both halves are always checked, and the same message is returned either
	// way: telling a guesser that the user exists halves their work.
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.user)) == 1
	passwordOK, err := verifyPassword(a.hash, password)
	if err != nil {
		logErrorf("%s: %v", _t("err auth hash"), err)
	}

	if !userOK || !passwordOK {
		a.sessions.recordFailure(addr)
		logErrorf("%s: %s", _t("err login failed"), addr)
		a.render(w, r, http.StatusUnauthorized, "login",
			loginPage{Error: _t("err login failed"), Insecure: !a.isSecure(r)})
		return
	}

	id, _, err := a.sessions.create(a.user)
	if err != nil {
		httpError(w, r, http.StatusInternalServerError)
		return
	}
	a.sessions.clearFailures(addr)
	logError(_t("login ok"), addr)

	secure := a.isSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName(r),
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		// Strict rather than Lax: nothing links into the panel from elsewhere,
		// so a request arriving from another site is never legitimate.
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionMax.Seconds()),
	})
	http.Redirect(w, r, adminPath+"/", http.StatusSeeOther)
}

func (a *adminPanel) serveLogout(w http.ResponseWriter, r *http.Request, sess *session, id string) {
	// POST only, with the token: a GET would let any page log the user out by
	// pointing an image at it.
	if !a.checkCSRF(r, sess) {
		httpError(w, r, http.StatusForbidden)
		return
	}

	a.sessions.destroy(id)
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName(r),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.isSecure(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, adminPath+"/login", http.StatusSeeOther)
}

// ── dashboard ───────────────────────────────────────────────────────────────

func (a *adminPanel) serveDashboard(w http.ResponseWriter, r *http.Request, sess *session, notice string) {
	page := a.dashboard(sess)
	page.Notice = notice
	a.render(w, r, http.StatusOK, "dashboard", page)
}

func (a *adminPanel) dashboard(sess *session) dashboardPage {
	page := dashboardPage{
		CSRF:       sess.csrf,
		User:       sess.user,
		ConfigPath: a.configPath,
		Jobs:       jobKinds,
		Busy:       a.jobs.busy(),
	}

	config, err := loadConfigFile(a.configPath)
	if err != nil {
		page.Error = err.Error()
		// Still render the form, from the defaults, so a config.toml that has
		// been broken by hand can be fixed from here rather than only from a
		// shell.
		defaults := defaultConfig()
		config = &defaults
	}
	page.Form = formFromConfig(config)

	if run := a.jobs.snapshot(); run != nil {
		page.Run = run
		page.RunLog = jobLogText(run)
	}
	return page
}

// serveSave validates the submitted form and writes config.toml.
func (a *adminPanel) serveSave(w http.ResponseWriter, r *http.Request, sess *session) {
	if !a.checkCSRF(r, sess) {
		httpError(w, r, http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		httpError(w, r, http.StatusBadRequest)
		return
	}

	config, err := configFromForm(r, a)
	if err != nil {
		page := a.dashboard(sess)
		page.Form = formFromRequest(r)
		page.Error = err.Error()
		a.render(w, r, http.StatusBadRequest, "dashboard", page)
		return
	}

	if err := saveConfigFile(a.configPath, config); err != nil {
		page := a.dashboard(sess)
		page.Form = formFromRequest(r)
		page.Error = err.Error()
		a.render(w, r, http.StatusBadRequest, "dashboard", page)
		return
	}

	logError(_t("config saved"), a.configPath)
	// PRG: a reload must not resubmit the form.
	http.Redirect(w, r, adminPath+"/?saved=1", http.StatusSeeOther)
}

// serveRun starts one action.
func (a *adminPanel) serveRun(w http.ResponseWriter, r *http.Request, sess *session) {
	if !a.checkCSRF(r, sess) {
		httpError(w, r, http.StatusForbidden)
		return
	}

	kind := r.PostFormValue("job")
	if !knownJob(kind) {
		httpError(w, r, http.StatusBadRequest)
		return
	}

	if err := a.jobs.start(jobKind(kind)); err != nil {
		page := a.dashboard(sess)
		if errors.Is(err, errJobBusy) {
			page.Error = _t("err job busy")
		} else {
			page.Error = err.Error()
		}
		a.render(w, r, http.StatusConflict, "dashboard", page)
		return
	}

	http.Redirect(w, r, adminPath+"/log", http.StatusSeeOther)
}

// serveVerify checks the configured package names against the cached index and
// renders the answer on the dashboard.
//
// Synchronous rather than a background job: it only reads what -di already
// cached, so it is over before the response would have been written anyway, and
// the answer belongs next to the field it is about.
func (a *adminPanel) serveVerify(w http.ResponseWriter, r *http.Request, sess *session) {
	if !a.checkCSRF(r, sess) {
		httpError(w, r, http.StatusForbidden)
		return
	}

	page := a.dashboard(sess)

	config, err := loadConfigFile(a.configPath)
	if err != nil {
		page.Error = err.Error()
		a.render(w, r, http.StatusBadRequest, "dashboard", page)
		return
	}

	result, err := verifyPackages(config)
	if err != nil {
		// Almost always "no index yet", which the hint on the button already
		// explains: say what happened and leave the page usable.
		page.Error = _t("err r indexes") + ": " + err.Error()
		a.render(w, r, http.StatusOK, "dashboard", page)
		return
	}

	page.Verify = result
	a.render(w, r, http.StatusOK, "dashboard", page)
}

// servePackages lists what is actually in the repository, and adopts or drops a
// name from [destination].packages.
func (a *adminPanel) servePackages(w http.ResponseWriter, r *http.Request, sess *session) {
	notice := ""

	if r.Method == http.MethodPost {
		if !a.checkCSRF(r, sess) {
			httpError(w, r, http.StatusForbidden)
			return
		}

		name := r.PostFormValue("name")
		add := r.PostFormValue("declare") == "add"

		if name == "" {
			httpError(w, r, http.StatusBadRequest)
			return
		}

		if err := declarePackage(a.configPath, name, add); err != nil {
			a.renderPackages(w, r, sess, "", err.Error())
			return
		}

		// PRG, keeping the page and filter the user was on, so adopting a name
		// does not throw them back to the top of a long list.
		http.Redirect(w, r, adminPath+"/packages?"+r.URL.RawQuery, http.StatusSeeOther)
		return
	}

	a.renderPackages(w, r, sess, notice, "")
}

// renderPackages scans the repository and renders one page of it.
func (a *adminPanel) renderPackages(w http.ResponseWriter, r *http.Request, sess *session, notice, failure string) {
	page := packagesPage{
		CSRF:   sess.csrf,
		User:   sess.user,
		Notice: notice,
		Error:  failure,
	}

	root, declared, err := repoRoot(a.configPath)
	if err != nil {
		page.Error = err.Error()
		a.render(w, r, http.StatusOK, "packages", page)
		return
	}

	items, err := scanPackages(root, declared)
	if err != nil {
		page.Error = err.Error()
		a.render(w, r, http.StatusOK, "packages", page)
		return
	}

	filter := inventoryFilter{
		Query:      strings.TrimSpace(r.URL.Query().Get("q")),
		Undeclared: r.URL.Query().Get("undeclared") != "",
	}

	number, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil {
		number = 1
	}

	page.List = paginate(items, filter, number)
	page.Root = root
	a.render(w, r, http.StatusOK, "packages", page)
}

// serveAvailable lists everything the mirror offers, so a package can be found
// and added to the list without knowing its name beforehand.
func (a *adminPanel) serveAvailable(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method == http.MethodPost {
		if !a.checkCSRF(r, sess) {
			httpError(w, r, http.StatusForbidden)
			return
		}

		name := r.PostFormValue("name")
		if name == "" {
			httpError(w, r, http.StatusBadRequest)
			return
		}

		if err := declarePackage(a.configPath, name, r.PostFormValue("declare") == "add"); err != nil {
			a.renderAvailable(w, r, sess, err.Error())
			return
		}

		http.Redirect(w, r, adminPath+"/available?"+r.URL.RawQuery, http.StatusSeeOther)
		return
	}

	a.renderAvailable(w, r, sess, "")
}

func (a *adminPanel) renderAvailable(w http.ResponseWriter, r *http.Request, sess *session, failure string) {
	page := availablePageData{CSRF: sess.csrf, User: sess.user, Error: failure}

	config, err := loadConfigFile(a.configPath)
	if err != nil {
		page.Error = err.Error()
		a.render(w, r, http.StatusOK, "available", page)
		return
	}

	items, err := a.catalog.load(config)
	if err != nil {
		// Almost always "-di has not been run yet", which the page explains
		// rather than showing an empty table as if the mirror were empty.
		page.List.Stale = true
		a.render(w, r, http.StatusOK, "available", page)
		return
	}

	filter := availableFilter{
		Query:    strings.TrimSpace(r.URL.Query().Get("q")),
		Declared: r.URL.Query().Get("declared") != "",
	}

	number, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil {
		number = 1
	}

	page.List = paginateAvailable(items, config.Destination.Packages, filter, number)
	page.Declared = len(config.Destination.Packages)
	a.render(w, r, http.StatusOK, "available", page)
}

// serveLog is the live view of the running action. It refreshes itself with a
// meta tag rather than a script, so the panel still needs no JavaScript.
func (a *adminPanel) serveLog(w http.ResponseWriter, r *http.Request, sess *session) {
	run := a.jobs.snapshot()
	page := logPage{CSRF: sess.csrf}

	if run != nil {
		page.Run = run
		page.Log = jobLogText(run)
		page.Refresh = run.Running
	}
	a.render(w, r, http.StatusOK, "log", page)
}

// ── form ────────────────────────────────────────────────────────────────────

// configForm is the panel's view of config.toml: every field as the string the
// browser sends, so a rejected submission can be redisplayed exactly as it was
// typed instead of being silently normalised.
type configForm struct {
	Type     string
	Source   string
	Arch     string
	Path     string
	Packages string

	Listen      string
	BehindProxy bool
	Health      bool
	PanelOn     bool
	User        string

	ProxyUse  bool
	ProxyHost string
	ProxyPort string

	Debug                  bool
	VerifyChecksum         bool
	SkipDownloadSameSize   bool
	MaxConcurrentDownloads string
	FilesDatabase          bool
	OnDemand               bool
}

func formFromConfig(config *Config) configForm {
	return configForm{
		Type:                   config.Server.Type,
		Source:                 strings.Join(config.Server.Source, "\n"),
		Arch:                   strings.Join(config.Destination.Arch, "\n"),
		Path:                   config.Destination.Path,
		Packages:               strings.Join(config.Destination.Packages, "\n"),
		Listen:                 config.Web.Listen,
		BehindProxy:            config.Web.BehindProxy,
		Health:                 config.Web.Health,
		PanelOn:                config.Web.Config,
		User:                   config.Web.User,
		ProxyUse:               config.Proxy.Use,
		ProxyHost:              config.Proxy.Host,
		ProxyPort:              strconv.Itoa(config.Proxy.Port),
		Debug:                  config.Settings.Debug,
		VerifyChecksum:         config.Settings.VerifyChecksum,
		SkipDownloadSameSize:   config.Settings.SkipDownloadSameSize,
		MaxConcurrentDownloads: strconv.Itoa(config.Settings.MaxConcurrentDownloads),
		FilesDatabase:          config.Settings.FilesDatabase,
		OnDemand:               config.Settings.OnDemand,
	}
}

// formFromRequest rebuilds the form from what was submitted, so a rejected save
// comes back with the user's own text rather than with what is still on disk.
func formFromRequest(r *http.Request) configForm {
	return configForm{
		Type:                   r.PostFormValue("type"),
		Source:                 r.PostFormValue("source"),
		Arch:                   r.PostFormValue("arch"),
		Path:                   r.PostFormValue("path"),
		Packages:               r.PostFormValue("packages"),
		Listen:                 r.PostFormValue("listen"),
		BehindProxy:            r.PostFormValue("behindProxy") != "",
		Health:                 r.PostFormValue("health") != "",
		PanelOn:                r.PostFormValue("panel") != "",
		User:                   r.PostFormValue("user"),
		ProxyUse:               r.PostFormValue("proxyUse") != "",
		ProxyHost:              r.PostFormValue("proxyHost"),
		ProxyPort:              r.PostFormValue("proxyPort"),
		Debug:                  r.PostFormValue("debug") != "",
		VerifyChecksum:         r.PostFormValue("verifyChecksum") != "",
		SkipDownloadSameSize:   r.PostFormValue("skipDownloadSameSize") != "",
		MaxConcurrentDownloads: r.PostFormValue("maxConcurrentDownloads"),
		FilesDatabase:          r.PostFormValue("filesDatabase") != "",
		OnDemand:               r.PostFormValue("onDemand") != "",
	}
}

// configFromForm turns a submission into a Config. The numeric fields are
// parsed here so the error names the field; everything else is validateConfig's
// job, which is the same check the command line runs.
func configFromForm(r *http.Request, a *adminPanel) (*Config, error) {
	form := formFromRequest(r)
	config := defaultConfig()

	config.Server.Type = strings.TrimSpace(form.Type)
	config.Server.Source = parseLines(form.Source)
	config.Destination.Arch = parseLines(form.Arch)
	config.Destination.Path = strings.TrimSpace(form.Path)
	config.Destination.Packages = parseLines(form.Packages)

	config.Web.Listen = strings.TrimSpace(form.Listen)
	config.Web.BehindProxy = form.BehindProxy
	config.Web.Health = form.Health
	config.Web.Config = form.PanelOn
	config.Web.User = strings.TrimSpace(form.User)

	// Read straight from the request: configForm deliberately never carries a
	// password, so it cannot be echoed back into the page. An empty field keeps
	// the stored hash; otherwise the plaintext is hashed here and goes no
	// further.
	if password := r.PostFormValue("newPassword"); password != "" {
		if len(password) < 8 {
			return nil, errors.New(_t("err password short"))
		}
		hash, err := hashPassword(password)
		if err != nil {
			return nil, err
		}
		config.Web.PasswordHash = hash
	} else {
		config.Web.PasswordHash = a.hash
	}

	config.Proxy.Use = form.ProxyUse
	config.Proxy.Host = strings.TrimSpace(form.ProxyHost)
	port, err := strconv.Atoi(strings.TrimSpace(form.ProxyPort))
	if err != nil {
		return nil, errors.New("[proxy].port: " + _t("out of range"))
	}
	config.Proxy.Port = port

	config.Settings.Debug = form.Debug
	config.Settings.VerifyChecksum = form.VerifyChecksum
	config.Settings.SkipDownloadSameSize = form.SkipDownloadSameSize
	config.Settings.FilesDatabase = form.FilesDatabase
	config.Settings.OnDemand = form.OnDemand

	max, err := strconv.Atoi(strings.TrimSpace(form.MaxConcurrentDownloads))
	if err != nil {
		return nil, errors.New("[settings].maxConcurrentDownloads: " + _t("out of range"))
	}
	config.Settings.MaxConcurrentDownloads = max

	return &config, nil
}
