package tinyrepo

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// healthServer publishes a repository with the health endpoint on, and returns
// the counters behind it so a test can read them from the other side too.
func healthServer(t *testing.T, root string, od *onDemand) (*httptest.Server, *serverStats) {
	t.Helper()

	stats := newServerStats()
	if od != nil {
		od.stats = stats
	}

	server := httptest.NewServer(repoHandler{
		root:     root,
		onDemand: od,
		health:   true,
		repoType: BackendDebian,
		stats:    stats,
	})
	t.Cleanup(server.Close)

	return server, stats
}

// healthGet fetches the endpoint and decodes it, which is also the assertion
// that what it publishes is JSON at all.
func healthGet(t *testing.T, server *httptest.Server) (*http.Response, healthReport) {
	t.Helper()

	response, err := http.Get(server.URL + healthPath)
	if err != nil {
		t.Fatalf("GET %s: %v", healthPath, err)
	}
	defer response.Body.Close()

	var report healthReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatalf("GET %s: the body does not decode as JSON: %v", healthPath, err)
	}
	return response, report
}

// Off by default is the whole point of the configuration key: a server that
// publishes it anyway would be answering on a path nobody agreed to.
func TestHealthIsNotPublishedUnlessEnabled(t *testing.T) {
	root := webTestRepo(t)

	server := httptest.NewServer(repoHandler{root: root})
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + healthPath)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("with [web].health off, %s answered %d, want %d",
			healthPath, response.StatusCode, http.StatusNotFound)
	}
}

func TestHealthReportsRequestCounters(t *testing.T) {
	root := webTestRepo(t)
	server, _ := healthServer(t, root, nil)

	for range 3 {
		response, err := http.Get(server.URL + "/pool/main/n/nano/nano_7.2-1_amd64.deb")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}

	response, err := http.Get(server.URL + "/pool/main/n/nano/absent.deb")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	httpResponse, report := healthGet(t, server)

	if httpResponse.StatusCode != http.StatusOK {
		t.Errorf("status %d, want %d", httpResponse.StatusCode, http.StatusOK)
	}
	if report.Status != "ok" {
		t.Errorf("status %q, want %q", report.Status, "ok")
	}
	if report.Requests.Hits != 3 {
		t.Errorf("hits %d, want 3", report.Requests.Hits)
	}
	if report.Requests.NotFound != 1 {
		t.Errorf("notFound %d, want 1", report.Requests.NotFound)
	}
	if report.Repo.Path != root || report.Repo.Type != BackendDebian {
		t.Errorf("repo %q/%q, want %q/%q", report.Repo.Type, report.Repo.Path, BackendDebian, root)
	}
	if report.Repo.OnDemand {
		t.Error("on demand is off, the report says it is on")
	}
	if report.Downloads != nil {
		t.Errorf("on demand is off, the report still carries downloads: %+v", report.Downloads)
	}
}

// A monitor polling every few seconds would otherwise be indistinguishable from
// repository traffic, and every counter would climb on its own.
func TestHealthDoesNotCountItself(t *testing.T) {
	root := webTestRepo(t)
	server, _ := healthServer(t, root, nil)

	for range 5 {
		healthGet(t, server)
	}

	_, report := healthGet(t, server)

	if report.Requests.Hits != 0 || report.Requests.Misses != 0 || report.Requests.NotFound != 0 {
		t.Errorf("polling the endpoint moved the counters: hits=%d misses=%d notFound=%d",
			report.Requests.Hits, report.Requests.Misses, report.Requests.NotFound)
	}
}

// The failure the endpoint exists to catch: still answering, no longer serving
// a repository.
func TestHealthIsDegradedWhenTheRepositoryIsGone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}

	server, _ := healthServer(t, root, nil)

	if _, report := healthGet(t, server); report.Status != "ok" {
		t.Fatalf("status %q before removing the repository, want %q", report.Status, "ok")
	}

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	response, report := healthGet(t, server)

	if report.Status != "degraded" {
		t.Errorf("status %q with the repository gone, want %q", report.Status, "degraded")
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d with the repository gone, want %d",
			response.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestHealthSetsItsHeaders(t *testing.T) {
	root := webTestRepo(t)
	server, _ := healthServer(t, root, nil)

	response, _ := healthGet(t, server)

	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type %q, want application/json", got)
	}
	// A cached health report is a stale health report.
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want %q", got, "no-store")
	}
}

// Nothing here is worth a panic: the counters exist to describe a running
// server, not to be a prerequisite for one.
func TestHealthSurvivesWithoutCounters(t *testing.T) {
	root := webTestRepo(t)

	server := httptest.NewServer(repoHandler{root: root, health: true})
	t.Cleanup(server.Close)

	response, report := healthGet(t, server)

	if response.StatusCode != http.StatusOK {
		t.Errorf("status %d, want %d", response.StatusCode, http.StatusOK)
	}
	if report.Uptime != 0 {
		t.Errorf("uptime %d with no counters, want 0", report.Uptime)
	}
	if report.Connections != nil {
		t.Errorf("connections reported with no listener behind them: %+v", report.Connections)
	}
}

func TestHealthReportsConnectionLimits(t *testing.T) {
	root := webTestRepo(t)
	server, stats := healthServer(t, root, nil)

	stats.conns = func() (int, int) { return 7, MaxConnections }

	_, report := healthGet(t, server)

	if report.Connections == nil {
		t.Fatal("no connections in the report")
	}
	if report.Connections.Open != 7 || report.Connections.Max != MaxConnections {
		t.Errorf("connections open=%d max=%d, want 7 and %d",
			report.Connections.Open, report.Connections.Max, MaxConnections)
	}
}

// The listener is the source of the numbers, so what it enforces and what the
// report shows cannot disagree.
func TestLimitListenerCountsMatchTheCap(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newLimitListener(inner, 3)
	defer listener.Close()

	if open, max := listener.counts(); open != 0 || max != 3 {
		t.Fatalf("a fresh listener counts open=%d max=%d, want 0 and 3", open, max)
	}

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}

	if open, _ := listener.counts(); open != 1 {
		t.Errorf("with one connection accepted, counts open=%d, want 1", open)
	}

	accepted.Close()

	if open, _ := listener.counts(); open != 0 {
		t.Errorf("after closing it, counts open=%d, want 0", open)
	}
}

func TestHealthReportsOnDemandDownloads(t *testing.T) {
	fixture := newOnDemandFixture(t, map[string]string{
		"pool/main/n/nano/nano_7.2-1_amd64.deb": "nano payload",
	}, nil)

	server, _ := healthServer(t, fixture.root, fixture.od)

	response, err := http.Get(server.URL + "/pool/main/n/nano/nano_7.2-1_amd64.deb")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("the package was answered %d, want %d", response.StatusCode, http.StatusOK)
	}

	_, report := healthGet(t, server)

	if !report.Repo.OnDemand {
		t.Error("on demand is on, the report says it is off")
	}
	if report.Repo.Catalog != 1 {
		t.Errorf("catalog %d, want 1", report.Repo.Catalog)
	}
	if report.Requests.Misses != 1 {
		t.Errorf("misses %d, want 1", report.Requests.Misses)
	}
	if report.Downloads == nil {
		t.Fatal("on demand is on, the report carries no downloads")
	}
	if report.Downloads.OK != 1 || report.Downloads.Failed != 0 {
		t.Errorf("downloads ok=%d failed=%d, want 1 and 0", report.Downloads.OK, report.Downloads.Failed)
	}
	if report.Downloads.InFlight != 0 {
		t.Errorf("downloads inFlight=%d once it finished, want 0", report.Downloads.InFlight)
	}
	if report.Downloads.Slots != fixture.od.config.Settings.MaxConcurrentDownloads {
		t.Errorf("downloads slots=%d, want %d",
			report.Downloads.Slots, fixture.od.config.Settings.MaxConcurrentDownloads)
	}
}

// A mirror that refuses has to show up as a failure rather than quietly not
// appearing at all, which is the reason to publish the counter.
func TestHealthCountsAFailedDownload(t *testing.T) {
	fixture := newOnDemandFixture(t, map[string]string{
		"pool/main/n/nano/nano_7.2-1_amd64.deb": "nano payload",
	}, nil)

	// In the catalog, absent from the mirror.
	fixture.od.cat.add(fixture.od.cat.Arches[0], "ghost", manifestEntry{
		BaseURL:  fixture.mirror.url(),
		PoolPath: "pool/main/g/ghost/ghost_1.0_amd64.deb",
		Size:     10,
	})

	server, _ := healthServer(t, fixture.root, fixture.od)

	response, err := http.Get(server.URL + "/pool/main/g/ghost/ghost_1.0_amd64.deb")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	_, report := healthGet(t, server)

	if report.Downloads == nil {
		t.Fatal("no downloads in the report")
	}
	if report.Downloads.Failed != 1 || report.Downloads.OK != 0 {
		t.Errorf("downloads failed=%d ok=%d, want 1 and 0", report.Downloads.Failed, report.Downloads.OK)
	}
}

// The gauge is what tells a saturated server from an idle one, so it has to
// come back down however the download ended.
func TestServerStatsGaugeReturnsToZero(t *testing.T) {
	stats := newServerStats()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			done := stats.fetchStart()
			if i%2 == 0 {
				done(nil)
				return
			}
			done(errors.New("mirror said no"))
		}()
	}
	wg.Wait()

	if got := stats.fetchNow.Load(); got != 0 {
		t.Errorf("inFlight %d after everything finished, want 0", got)
	}
	if got := stats.fetchOK.Load(); got != 10 {
		t.Errorf("ok %d, want 10", got)
	}
	if got := stats.fetchFailed.Load(); got != 10 {
		t.Errorf("failed %d, want 10", got)
	}
}

// A nil serverStats is what every handler built by hand has, so each counter
// has to tolerate it rather than only the ones a test happened to reach.
func TestServerStatsToleratesNil(t *testing.T) {
	var stats *serverStats

	stats.hit()
	stats.miss()
	stats.missing()
	stats.busy()
	stats.fetchStart()(nil)
	stats.fetchStart()(errors.New("mirror said no"))
}
