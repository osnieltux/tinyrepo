package main

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// hiddenNames live inside the destination path but are not part of the
// published repository: the upstream caches and tinyrepo's own state. Serving
// them would work, but it would also publish a second copy of every index under
// a path that looks like a repository and is not one.
var hiddenNames = map[string]bool{
	ManifestDirName: true, // .tinyrepo
	DistsCacheName:  true, // dists_cache
	ArchCacheName:   true, // db_cache
}

// repoHandler serves the generated repository over HTTP.
type repoHandler struct {
	root string
	// onDemand is nil unless [settings].onDemand is on, in which case a file
	// that is missing locally may still be one the published index promises.
	onDemand *onDemand
	// behindProxy mirrors [web].behindProxy: only then is X-Forwarded-Proto
	// worth believing.
	behindProxy bool
	// health mirrors [web].health: whether healthPath is answered at all.
	health bool
	// repoType is only reported by the health endpoint.
	repoType string
	// stats is nil in the tests that build this by hand; every counter tolerates
	// that.
	stats *serverStats
}

// newHandler builds the handler serveRepo publishes, with everything the
// configuration decides about it.
func newHandler(config *Config, od *onDemand, stats *serverStats) http.Handler {
	return repoHandler{
		root:        config.Destination.Path,
		onDemand:    od,
		behindProxy: config.Web.BehindProxy,
		health:      config.Web.Health,
		repoType:    config.Server.Type,
		stats:       stats,
	}
}

func (h repoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpError(w, r, http.StatusMethodNotAllowed)
		return
	}

	// Before the filesystem, so nothing on disk can shadow it, and before the
	// counters, so a monitor polling every few seconds does not show up as
	// repository traffic.
	if h.health && r.URL.Path == healthPath {
		h.serveHealth(w, r)
		return
	}

	urlPath, ok := cleanRequestPath(r.URL.Path)
	if !ok {
		h.stats.missing()
		httpError(w, r, http.StatusNotFound)
		return
	}

	target := filepath.Join(h.root, filepath.FromSlash(strings.TrimPrefix(urlPath, "/")))

	info, err := os.Stat(target)
	if err != nil {
		// The one hook on-demand mode needs: a file we do not have may still be
		// a package the published index promises, in which case it is fetched
		// now and streamed straight through to the client.
		if h.onDemand.serveMiss(w, r, strings.TrimPrefix(urlPath, "/"), target) {
			h.stats.miss()
			return
		}
		h.stats.missing()
		httpError(w, r, http.StatusNotFound)
		return
	}

	if info.IsDir() {
		// Without the trailing slash every relative link on the page would
		// resolve one level too high.
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		h.listDirectory(w, r, urlPath, target)
		return
	}

	file, err := os.Open(target)
	if err != nil {
		h.stats.missing()
		httpError(w, r, http.StatusNotFound)
		return
	}
	defer file.Close()

	// ServeContent gives conditional requests and byte ranges, which apt and
	// pacman both use to resume an interrupted download.
	h.stats.hit()
	logRequest(r, http.StatusOK)
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

// cleanRequestPath normalises a URL path and refuses anything that escapes the
// repository or reaches into a hidden directory.
func cleanRequestPath(urlPath string) (string, bool) {
	// Clean on an absolute path resolves every ".." against "/", so the result
	// can never climb above the root.
	cleaned := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))

	for _, element := range strings.Split(cleaned, "/") {
		if element == "" {
			continue
		}
		if hiddenName(element) {
			return "", false
		}
	}
	return cleaned, true
}

// hiddenName reports whether a path element must stay out of the published
// repository. The TempSuffix case only matters on demand, where downloading and
// serving happen at the same time: without it a client could pick up a
// half-written package as if it were the real one.
func hiddenName(name string) bool {
	return hiddenNames[name] ||
		strings.HasPrefix(name, ".") ||
		strings.HasSuffix(name, TempSuffix)
}

func httpError(w http.ResponseWriter, r *http.Request, status int) {
	logRequest(r, status)
	http.Error(w, http.StatusText(status), status)
}

func logRequest(r *http.Request, status int) {
	logDebugf("%s %s %s %d", r.RemoteAddr, r.Method, r.URL.Path, status)
}

// listingEntry is one row of a directory listing.
type listingEntry struct {
	Name string // display name, with a trailing slash for directories
	// Href is the display name behind "./". Without that prefix a pacman
	// package carrying an epoch, such as "lz4-1:1.10.0-2-x86_64.pkg.tar.zst",
	// reads as a URL scheme and the link breaks.
	Href    string
	Size    string
	ModTime string
}

// listingPage is everything the directory template needs.
type listingPage struct {
	Path    string
	Entries []listingEntry
	IsRoot  bool
	Apt     string // set when the root holds a Debian repository
	Pacman  string // set when the root holds a pacman repository
}

// Classic HTML on purpose: no CSS, no scripts, nothing a plain text browser
// cannot render.
var listingTemplate = template.Must(template.New("listing").Parse(`<!DOCTYPE html>
<html>
<head>
<title>Index of {{.Path}}</title>
</head>
<body>
<h1>Index of {{.Path}}</h1>
<hr>
<table>
<tr><th align="left">Name</th><th align="right">Size</th><th align="left">Last modified</th></tr>
{{if not .IsRoot}}<tr><td><a href="../">../</a></td><td align="right">-</td><td></td></tr>
{{end}}{{range .Entries}}<tr><td><a href="{{.Href}}">{{.Name}}</a></td><td align="right">{{.Size}}</td><td>{{.ModTime}}</td></tr>
{{end}}</table>
<hr>
{{if .Apt}}<p>Add this repository to <tt>/etc/apt/sources.list</tt>:</p>
<pre>{{.Apt}}</pre>
{{end}}{{if .Pacman}}<p>Add this repository to <tt>/etc/pacman.conf</tt>:</p>
<pre>{{.Pacman}}</pre>
{{end}}<address>tinyrepo</address>
</body>
</html>
`))

func (h repoHandler) listDirectory(w http.ResponseWriter, r *http.Request, urlPath, dir string) {
	names, err := os.ReadDir(dir)
	if err != nil {
		httpError(w, r, http.StatusNotFound)
		return
	}

	// path.Clean drops the trailing slash, but a directory should be shown -
	// and read - as a directory.
	display := urlPath
	if !strings.HasSuffix(display, "/") {
		display += "/"
	}

	page := listingPage{Path: display, IsRoot: display == "/"}

	for _, entry := range names {
		if hiddenName(entry.Name()) {
			continue
		}

		row := listingEntry{Name: entry.Name(), Size: "-"}
		if entry.IsDir() {
			row.Name += "/"
		}
		row.Href = "./" + row.Name

		if info, err := entry.Info(); err == nil {
			row.ModTime = info.ModTime().Format("2006-01-02 15:04")
			if !entry.IsDir() {
				row.Size = humanSize(info.Size())
			}
		}
		page.Entries = append(page.Entries, row)
	}

	// Directories first, then by name, so the listing is stable and readable.
	sort.SliceStable(page.Entries, func(i, j int) bool {
		iDir := strings.HasSuffix(page.Entries[i].Name, "/")
		jDir := strings.HasSuffix(page.Entries[j].Name, "/")
		if iDir != jDir {
			return iDir
		}
		return page.Entries[i].Name < page.Entries[j].Name
	})

	if page.IsRoot {
		page.Apt, page.Pacman = h.clientHints(r)
	}

	logRequest(r, http.StatusOK)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := listingTemplate.Execute(w, page); err != nil {
		logErrorf("%s: %v", _t("err serving"), err)
	}
}

// requestScheme is the scheme a client should use to reach this repository.
//
// Behind a proxy that terminates TLS the connection tinyrepo sees is plain
// http, so the scheme has to come from the proxy's header - otherwise the home
// page would hand out an "http://" address for a repository published over
// https, sending apt to port 80 in the clear. That header is only believed when
// the configuration says there is a proxy: reachable directly, anyone could
// otherwise dictate the address the page tells people to trust.
func requestScheme(r *http.Request, behindProxy bool) string {
	if behindProxy {
		// A chain of proxies appends to the header; the first entry is the one
		// that faced the client.
		forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")

		switch strings.ToLower(strings.TrimSpace(forwarded)) {
		case "https":
			return "https"
		case "http":
			return "http"
		}
	}

	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// clientHints returns the sources.list or pacman.conf snippet for whichever
// repository was actually generated, so the root page tells a visitor what to
// do with it.
func (h repoHandler) clientHints(r *http.Request) (apt string, pacman string) {
	base := requestScheme(r, h.behindProxy) + "://" + r.Host + "/"

	if fileExists(filepath.Join(h.root, "dists", DestinationDistsName, "Release")) {
		apt = fmt.Sprintf("deb [trusted=yes] %s %s %s", base, DestinationDistsName, DestinationComponent)
	}
	if fileExists(filepath.Join(h.root, DestinationDistsName+".db")) {
		pacman = fmt.Sprintf("[%s]\nSigLevel = Optional TrustAll\nServer = %s", DestinationDistsName, base)
	}
	return apt, pacman
}

// humanSize renders a byte count the way a classic directory index does.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d", n)
	}

	value, exponent := float64(n)/unit, 0
	for value >= unit && exponent < 4 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f%c", value, "KMGTP"[exponent])
}

// webDisplayURL turns a listener address into a URL a user can paste. A
// wildcard listener reports itself as "[::]", which is not a usable host.
func webDisplayURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String() + "/"
	}

	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// serveRepo publishes the destination directory over HTTP until interrupted.
func serveRepo(config *Config, backend Backend) error {
	root := config.Destination.Path

	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s: %s", _t("err no repo"), root)
	}

	// Handlers get this context, not the request's: a download triggered by a
	// client that then disconnects still has to finish.
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()

	stats := newServerStats()

	var od *onDemand

	if config.Settings.OnDemand {
		od, err = newOnDemand(serverCtx, config, backend)
		if err != nil {
			return err
		}
		od.stats = stats
		defer od.stop()
	}

	plain, err := net.Listen("tcp", config.Web.Listen)
	if err != nil {
		return err
	}
	listener := newLimitListener(plain, MaxConnections)
	// Read from the listener rather than counted separately, so the number
	// reported cannot drift from the number actually enforced.
	stats.conns = listener.counts

	server := &http.Server{
		Handler:           newHandler(config, od, stats),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return serverCtx },
		// No WriteTimeout on purpose: a package can be hundreds of megabytes
		// over a slow link, and a deadline here would cut it off mid-download.
	}

	logError(_t("serving"), root, "->", webDisplayURL(listener.Addr()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()

	select {
	case err := <-failed:
		if err == http.ErrServerClosed {
			return nil
		}
		return err

	case <-ctx.Done():
		logError(_t("stopping"))

		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := server.Shutdown(shutdown)
		// Anything still streaming is cut here rather than kept waiting.
		cancelServer()

		// A large download still in flight is not a failure to shut down, and
		// should not turn Ctrl+C into a non-zero exit status.
		if err == context.DeadlineExceeded {
			return nil
		}
		return err
	}
}
