package tinyrepo

import (
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// healthPath is where the health report is published.
//
// It is matched before anything touches the filesystem, so a file of that name
// could never shadow it. Neither repository format has one - Debian puts
// everything under dists/ and pool/, pacman under the .db and the package files
// - which is what makes the name safe to take.
const healthPath = "/healthz"

// serverStats are the counters behind healthPath.
//
// -ws logs each request only with debug on, and a running server otherwise says
// nothing at all about whether it is working: whether packages are being
// fetched, whether the mirror is answering, whether it is shedding load. These
// are the numbers that answer that without turning on debug and reading a log.
//
// Every method is safe on a nil receiver, because the tests build handlers by
// hand and a counter is not worth a panic.
type serverStats struct {
	started time.Time

	hits     atomic.Int64 // served from disk
	misses   atomic.Int64 // taken by on demand
	notFound atomic.Int64

	fetchOK     atomic.Int64
	fetchFailed atomic.Int64
	fetchBusy   atomic.Int64 // turned away with 503, no slot free in time
	fetchNow    atomic.Int64 // gauge: downloads in flight

	// conns reports open and maximum connections. It reads the limited
	// listener's own slots rather than a second counter that could drift from
	// them. Nil when there is no such listener, which is every test.
	conns func() (open, max int)
}

func newServerStats() *serverStats {
	return &serverStats{started: time.Now()}
}

func (s *serverStats) hit() {
	if s != nil {
		s.hits.Add(1)
	}
}

func (s *serverStats) miss() {
	if s != nil {
		s.misses.Add(1)
	}
}

func (s *serverStats) missing() {
	if s != nil {
		s.notFound.Add(1)
	}
}

func (s *serverStats) busy() {
	if s != nil {
		s.fetchBusy.Add(1)
	}
}

// fetchStart marks a download as started and returns the function that records
// how it ended, so the gauge cannot be left raised by an early return.
func (s *serverStats) fetchStart() func(error) {
	if s == nil {
		return func(error) {}
	}

	s.fetchNow.Add(1)
	return func(err error) {
		s.fetchNow.Add(-1)
		if err != nil {
			s.fetchFailed.Add(1)
			return
		}
		s.fetchOK.Add(1)
	}
}

type healthReport struct {
	Status string `json:"status"`
	Uptime int64  `json:"uptimeSeconds"`

	Repo struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		OnDemand bool   `json:"onDemand"`
		// Catalog is how many files the published catalog promises, and only
		// means anything on demand.
		Catalog int `json:"catalog,omitempty"`
	} `json:"repo"`

	Requests struct {
		Hits     int64 `json:"hits"`
		Misses   int64 `json:"misses"`
		NotFound int64 `json:"notFound"`
	} `json:"requests"`

	// Downloads is absent when on demand is off: -ws downloads nothing then,
	// and zeroes would read as "nothing is being fetched" rather than "nothing
	// can be".
	Downloads *healthDownloads `json:"downloads,omitempty"`

	Connections *healthConnections `json:"connections,omitempty"`
}

type healthDownloads struct {
	OK       int64 `json:"ok"`
	Failed   int64 `json:"failed"`
	Busy     int64 `json:"busy"`
	InFlight int64 `json:"inFlight"`
	Slots    int   `json:"slots"`
}

type healthConnections struct {
	Open int `json:"open"`
	Max  int `json:"max"`
}

func (h repoHandler) serveHealth(w http.ResponseWriter, r *http.Request) {
	stats := h.stats
	if stats == nil {
		stats = &serverStats{}
	}

	var report healthReport
	report.Status = "ok"
	if !stats.started.IsZero() {
		report.Uptime = int64(time.Since(stats.started).Seconds())
	}

	report.Repo.Type = h.repoType
	report.Repo.Path = h.root
	report.Requests.Hits = stats.hits.Load()
	report.Requests.Misses = stats.misses.Load()
	report.Requests.NotFound = stats.notFound.Load()

	if h.onDemand != nil {
		report.Repo.OnDemand = true
		report.Repo.Catalog = len(h.onDemand.cat.Files)
		report.Downloads = &healthDownloads{
			OK:       stats.fetchOK.Load(),
			Failed:   stats.fetchFailed.Load(),
			Busy:     stats.fetchBusy.Load(),
			InFlight: stats.fetchNow.Load(),
			Slots:    h.onDemand.config.Settings.MaxConcurrentDownloads,
		}
	}

	if stats.conns != nil {
		open, max := stats.conns()
		report.Connections = &healthConnections{Open: open, Max: max}
	}

	// A repository whose directory has been moved or unmounted underneath us is
	// still answering requests and is still broken. Reporting it healthy is
	// exactly the failure this endpoint exists to catch, so it is the one thing
	// checked rather than merely counted.
	status := http.StatusOK
	if info, err := os.Stat(h.root); err != nil || !info.IsDir() {
		report.Status = "degraded"
		status = http.StatusServiceUnavailable
	}

	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		httpError(w, r, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A monitor asking every fifteen seconds must not be answered by a cache,
	// its own or a proxy's.
	w.Header().Set("Cache-Control", "no-store")

	logRequest(r, status)
	w.WriteHeader(status)
	w.Write(append(body, '\n'))
}
