package tinyrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// quietWriter forwards to w until the first write error and then silently
// swallows everything.
//
// It is what keeps a client disconnect from aborting the download: io.Copy
// stops at the first write error, so without this a client hitting Ctrl+C would
// throw away a package that was 90% fetched. The client that triggered the miss
// is not the reason to fetch it - the next request is.
type quietWriter struct {
	w      io.Writer
	failed bool
}

func (q *quietWriter) Write(p []byte) (int, error) {
	if q.failed || q.w == nil {
		return len(p), nil
	}
	if _, err := q.w.Write(p); err != nil {
		q.failed = true
	}
	return len(p), nil
}

// streamThrough downloads fileURL to dest while copying the same bytes to
// mirror, and only moves it into place once the size and SHA256 both match.
// A broken transfer therefore never leaves a file a later run would accept.
//
// It reports whether anything reached mirror, so a caller writing an HTTP
// response can still send a clean error when the mirror refused the request.
func streamThrough(ctx context.Context, fileURL, dest string, want fileCheck, mirror io.Writer) (sent bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return false, fmt.Errorf("%s: %v", _t("error d"), err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("%s: %v", _t("error d"), err)
	}
	defer resp.Body.Close()

	// Without this check a 404 error page gets written to disk, and served to
	// the client, as if it were the requested package.
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%s %s: %s", _t("unexpected status"), resp.Status, fileURL)
	}

	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return false, fmt.Errorf("%s %s: %v", _t("error c p"), dir, err)
	}

	// A unique temporary rather than writeFileAtomic's fixed name: a -dp in
	// another process may be fetching the same package at the same time. The
	// TempSuffix tail is what keeps it out of the published repository.
	tmp, err := os.CreateTemp(dir, filepath.Base(dest)+".*"+TempSuffix)
	if err != nil {
		return false, fmt.Errorf("%s: %v", _t("error c f"), err)
	}
	// A no-op once the rename below has succeeded.
	defer os.Remove(tmp.Name())

	// The index says how big this package is, so anything beyond that is a
	// mirror misbehaving and must be stopped as it arrives, not measured
	// afterwards from a full disk.
	limit := want.Size
	if limit <= 0 {
		limit = MaxIndexSize
	}
	if resp.ContentLength > limit {
		return false, fmt.Errorf("%s: %s (%d > %d)", _t("size mismatch"), fileURL, resp.ContentLength, limit)
	}

	quiet := &quietWriter{w: mirror}
	sum := sha256.New()

	written, copyErr := copyCapped(io.MultiWriter(tmp, sum, quiet), resp.Body, limit)
	sent = written > 0 && !quiet.failed

	if copyErr != nil {
		tmp.Close()
		return sent, fmt.Errorf("%s: %v", _t("error s c"), copyErr)
	}
	if err := tmp.Close(); err != nil {
		return sent, err
	}

	if want.Size > 0 && written != want.Size {
		return sent, fmt.Errorf("%s: %s (%d != %d)", _t("size mismatch"), fileURL, written, want.Size)
	}
	if want.SHA256 != "" {
		if got := hex.EncodeToString(sum.Sum(nil)); !strings.EqualFold(got, want.SHA256) {
			return sent, fmt.Errorf("%s: %s", _t("checksum mismatch"), fileURL)
		}
	}

	return sent, os.Rename(tmp.Name(), dest)
}

// inflight is one acquisition in progress. Waiters block on done and then read
// the finished file from disk; only the leader talks to the mirror.
type inflight struct {
	done chan struct{}
	// err is written by the leader before done is closed and read only after
	// it is closed, so the close is the happens-before edge and no lock is
	// needed around it.
	err error
}

func (in *inflight) wait(ctx context.Context) error {
	select {
	case <-in.done:
		return in.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acquirer serialises downloads by request path, so that ten clients asking for
// the same missing package at the same time produce one download, not ten.
type acquirer struct {
	mu     sync.Mutex
	active map[string]*inflight
}

// begin registers an acquisition. leader is true for the caller that has to
// perform it; every other caller waits on the returned inflight.
func (a *acquirer) begin(key string) (*inflight, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.active == nil {
		a.active = map[string]*inflight{}
	}
	if in, ok := a.active[key]; ok {
		return in, false
	}

	in := &inflight{done: make(chan struct{})}
	a.active[key] = in
	return in, true
}

// finish is called by the leader exactly once. The key is dropped before done
// is closed, so a request arriving in that window starts a fresh acquisition
// instead of attaching to one that has already ended.
func (a *acquirer) finish(key string, in *inflight, err error) {
	a.mu.Lock()
	delete(a.active, key)
	a.mu.Unlock()

	in.err = err
	close(in.done)
}

// prefetcher warms the dependencies of a package that was just fetched on
// demand, so that the requests which follow are hits. It is deliberately lossy:
// a full queue drops work rather than making a request handler wait.
type prefetcher struct {
	jobs chan manifestEntry

	mu   sync.Mutex
	seen map[string]bool // request path -> queued at some point

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	fetch  func(context.Context, manifestEntry) error
}

func newPrefetcher(parent context.Context, workers int, fetch func(context.Context, manifestEntry) error) *prefetcher {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(parent)

	p := &prefetcher{
		jobs:   make(chan manifestEntry, 16*workers),
		seen:   map[string]bool{},
		ctx:    ctx,
		cancel: cancel,
		fetch:  fetch,
	}

	for range workers {
		p.wg.Add(1)
		go p.work()
	}
	return p
}

func (p *prefetcher) work() {
	defer p.wg.Done()

	for {
		select {
		case <-p.ctx.Done():
			return
		case entry := <-p.jobs:
			if err := p.fetch(p.ctx, entry); err != nil {
				logDebugf("%s %s: %v", _t("err fetching"), entry.PoolPath, err)
			}
		}
	}
}

// enqueue never blocks, and never queues the same package twice.
func (p *prefetcher) enqueue(entries []manifestEntry) {
	if p == nil {
		return
	}

	for _, entry := range entries {
		p.mu.Lock()
		already := p.seen[entry.PoolPath]
		p.seen[entry.PoolPath] = true
		p.mu.Unlock()

		if already {
			continue
		}

		select {
		case p.jobs <- entry:
		default:
			// Best effort by definition: dropping a warm-up makes the next
			// request slower, blocking a handler makes it broken.
			logDebug(_t("queue full"), entry.PoolPath)
		}
	}
}

// stop cancels the workers and waits at most timeout for them to unwind. The
// channel is never closed, so an enqueue racing with shutdown cannot panic.
func (p *prefetcher) stop(timeout time.Duration) {
	if p == nil {
		return
	}
	p.cancel()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		// A worker still inside a large download must not hold up shutting
		// down. It only ever leaves a temporary file behind, which -cl removes.
	}
}

// onDemand is the state the web server needs to serve a package it does not
// have: the upstream catalog, one download per file, and a warm-up queue.
type onDemand struct {
	config *Config
	cat    *repoCatalog
	root   string

	// ctx is the server's context, not the request's. A download has to outlive
	// the client that triggered it.
	ctx     context.Context
	acquire acquirer
	// limit caps how many downloads run at once across every client. acquire
	// only collapses requests for the same file, which does nothing about one
	// client asking for a thousand different ones.
	limit *fetchLimit
	// wait is how long a request queues for a slot. Zero means fetchWait; a
	// test sets it short so it does not have to sit out the real one.
	wait time.Duration
	pre  *prefetcher
	// stats is the web server's counters, or nil when there is no server to
	// report to.
	stats *serverStats
}

// fetchWait is how long a request waits for a download slot before giving up
// and asking the client to come back. Long enough to ride out a busy moment,
// short enough that a saturated server sheds load instead of piling up
// connections that apt and pacman will abandon anyway.
const fetchWait = 30 * time.Second

func newOnDemand(ctx context.Context, config *Config, backend Backend) (*onDemand, error) {
	cat, err := backend.Catalog(config)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", _t("err catalog"), err)
	}

	o := &onDemand{
		config: config,
		cat:    cat,
		root:   config.Destination.Path,
		ctx:    ctx,
		limit:  newFetchLimit(config.Settings.MaxConcurrentDownloads),
	}
	o.pre = newPrefetcher(ctx, config.Settings.MaxConcurrentDownloads, o.fetch)

	logError(_t("on demand ready"), len(cat.Files))
	return o, nil
}

func (o *onDemand) stop() {
	if o == nil {
		return
	}
	o.pre.stop(2 * time.Second)
}

// fetch downloads one package in the background, through the same registry the
// request path uses so a warm-up and a live request never duplicate work.
func (o *onDemand) fetch(ctx context.Context, entry manifestEntry) error {
	dest, err := entry.localPath(o.root)
	if err != nil {
		return fmt.Errorf("%s %s: %v", _t("err unsafe path"), entry.PoolPath, err)
	}
	if entry.check(o.config).matchesLocal(dest) {
		return nil
	}

	in, leader := o.acquire.begin(entry.PoolPath)
	if !leader {
		return in.wait(ctx)
	}

	// Background work yields: if every slot is busy serving a real request,
	// this warm-up is skipped rather than queued.
	if !o.limit.acquire(ctx, 0) {
		o.acquire.finish(entry.PoolPath, in, nil)
		logDebug(_t("queue full"), entry.PoolPath)
		return nil
	}
	defer o.limit.release()

	done := o.stats.fetchStart()
	err = downloadFileContext(ctx, entry.sourceURL(), dest, entry.check(o.config))
	done(err)

	o.acquire.finish(entry.PoolPath, in, err)
	return err
}

// serveMiss handles a request for a file that is not on disk. It reports
// whether it took the request; anything the catalog does not know about is left
// to the caller's plain 404, which covers directories, Release and the
// generated databases.
func (o *onDemand) serveMiss(w http.ResponseWriter, r *http.Request, requestPath, dest string) bool {
	if o == nil {
		return false
	}

	entry, ok := o.cat.entry(requestPath)
	if !ok {
		return false
	}

	if r.Method == http.MethodHead {
		// Answered from the index. Downloading hundreds of megabytes because
		// somebody probed with HEAD would not be a reasonable trade, and both
		// apt and pacman only HEAD to size-check.
		writePackageHeader(w, entry.Size)
		logRequest(r, http.StatusOK)
		return true
	}

	in, leader := o.acquire.begin(requestPath)
	if !leader {
		// Someone else is already fetching it. Waiting and then serving from
		// disk is where byte ranges and conditional requests come from.
		if err := in.wait(r.Context()); err != nil {
			httpError(w, r, http.StatusNotFound)
			return true
		}
		o.serveLocal(w, r, dest)
		return true
	}

	// One client must not be able to open a download per package it can name.
	// Past the limit the request is told to come back rather than held open,
	// because apt and pacman both give up long before an unbounded queue would
	// drain.
	wait := o.wait
	if wait <= 0 {
		wait = fetchWait
	}
	if !o.limit.acquire(r.Context(), wait) {
		o.acquire.finish(requestPath, in, errBusy)
		o.stats.busy()
		w.Header().Set("Retry-After", "5")
		httpError(w, r, http.StatusServiceUnavailable)
		return true
	}
	defer o.limit.release()

	logDebug(_t("fetching"), requestPath)

	// The headers go out with the first byte, so a mirror that answers 404
	// still produces a clean error here instead of an empty 200.
	body := &lazyHeader{w: w, r: r, size: entry.Size}
	done := o.stats.fetchStart()
	sent, err := streamThrough(o.ctx, entry.sourceURL(), dest, entry.check(o.config), body)
	done(err)

	o.acquire.finish(requestPath, in, err)

	if err != nil {
		logErrorf("%s %s: %v", _t("err fetching"), requestPath, err)
		if !sent {
			httpError(w, r, http.StatusBadGateway)
		}
		return true
	}

	body.flush()

	// Warm the dependencies now rather than before the download: prefetching
	// while the client is still waiting would steal bandwidth from the request
	// in flight, and apt and pacman fetch sequentially anyway.
	o.pre.enqueue(o.cat.closure(requestPath))
	return true
}

// serveLocal serves a file that has just finished downloading.
func (o *onDemand) serveLocal(w http.ResponseWriter, r *http.Request, dest string) {
	info, err := os.Stat(dest)
	if err != nil {
		httpError(w, r, http.StatusNotFound)
		return
	}

	file, err := os.Open(dest)
	if err != nil {
		httpError(w, r, http.StatusNotFound)
		return
	}
	defer file.Close()

	logRequest(r, http.StatusOK)
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func writePackageHeader(w http.ResponseWriter, size int64) {
	// Go's mime table knows neither .deb nor .zst, so this is also what
	// http.ServeContent produces on the hit path. Matching it keeps a hit and a
	// miss indistinguishable to the client.
	w.Header().Set("Content-Type", "application/octet-stream")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
}

// lazyHeader writes the response header on the first byte of the body, so that
// a failure before then can still be reported as an HTTP error.
type lazyHeader struct {
	w     http.ResponseWriter
	r     *http.Request
	size  int64
	wrote bool
}

func (l *lazyHeader) Write(p []byte) (int, error) {
	l.flush()
	return l.w.Write(p)
}

func (l *lazyHeader) flush() {
	if l.wrote {
		return
	}
	l.wrote = true
	writePackageHeader(l.w, l.size)
	logRequest(l.r, http.StatusOK)
}
