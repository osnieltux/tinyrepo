package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The on-demand tests all run against a fake mirror, so they need no network
// and stay in -short. See integration_ondemand_test.go for the real thing.

// fakeMirror is an upstream that serves a fixed set of paths and counts every
// request, so a test can prove a download did or did not happen.
type fakeMirror struct {
	server   *httptest.Server
	requests atomic.Int64
	// release, when non-nil, blocks every response until it is closed. It is
	// how the deduplication test holds a download open.
	release chan struct{}
}

func newFakeMirror(t *testing.T, files map[string]string) *fakeMirror {
	t.Helper()

	mirror := &fakeMirror{}
	mirror.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirror.requests.Add(1)

		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		if mirror.release != nil {
			<-mirror.release
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		io.WriteString(w, body)
	}))
	t.Cleanup(mirror.server.Close)

	return mirror
}

func (m *fakeMirror) url() string { return m.server.URL }

// onDemandFixture wires a repository root, a fake mirror and a catalog whose
// dependency graph is given as "package -> its dependencies".
type onDemandFixture struct {
	root   string
	mirror *fakeMirror
	od     *onDemand
	server *httptest.Server
}

// newOnDemandFixture builds a one-architecture catalog from bodies keyed by
// request path, plus an optional dependency graph keyed by package name. A
// package's name is its request path with the extension stripped, which keeps
// the fixtures readable.
func newOnDemandFixture(t *testing.T, bodies map[string]string, deps map[string][]string) *onDemandFixture {
	t.Helper()

	mirror := newFakeMirror(t, bodies)
	root := t.TempDir()

	config := defaultConfig()
	config.Destination.Path = root
	config.Settings.MaxConcurrentDownloads = 2

	cat := newRepoCatalog()
	view := newArchCatalogView("amd64", newCatalog())

	for path, body := range bodies {
		name := packageNameForPath(path)

		var clauses [][]string
		for _, dependency := range deps[name] {
			clauses = append(clauses, []string{dependency})
		}
		view.Deps.addPackage(name, clauses, nil, nil)

		sum := sha256.Sum256([]byte(body))
		cat.add(view, name, manifestEntry{
			BaseURL:  mirror.url(),
			PoolPath: path,
			Size:     int64(len(body)),
			SHA256:   hex.EncodeToString(sum[:]),
		})
	}
	cat.Arches = append(cat.Arches, view)

	od := &onDemand{config: &config, cat: cat, root: root, ctx: context.Background()}
	od.pre = newPrefetcher(context.Background(), 2, od.fetch)
	t.Cleanup(func() { od.pre.stop(2 * time.Second) })

	fixture := &onDemandFixture{root: root, mirror: mirror, od: od}
	fixture.server = httptest.NewServer(repoHandler{root: root, onDemand: od})
	t.Cleanup(fixture.server.Close)

	return fixture
}

// packageNameForPath turns "pool/main/n/nano/nano_1.0_amd64.deb" into "nano".
func packageNameForPath(path string) string {
	base := path[strings.LastIndex(path, "/")+1:]
	if cut := strings.IndexAny(base, "_-."); cut > 0 {
		return base[:cut]
	}
	return base
}

func (f *onDemandFixture) get(t *testing.T, method, path string, headers map[string]string) *http.Response {
	t.Helper()

	request, err := http.NewRequest(method, f.server.URL+path, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { response.Body.Close() })

	return response
}

func (f *onDemandFixture) body(t *testing.T, response *http.Response) string {
	t.Helper()

	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	return string(data)
}

// onDisk reports the contents of a file inside the served repository.
func (f *onDemandFixture) onDisk(t *testing.T, path string) (string, bool) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(path)))
	if err != nil {
		return "", false
	}
	return string(data), true
}

const debPath = "pool/main/n/nano/nano_7.2-1_amd64.deb"

// A miss has to reach the mirror, reach the client, and land on disk. If any of
// the three is missing the mode is pointless.
func TestOnDemandServesMissFromUpstream(t *testing.T) {
	const body = "the nano package"

	fixture := newOnDemandFixture(t, map[string]string{debPath: body}, nil)

	response := fixture.get(t, http.MethodGet, "/"+debPath, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := fixture.body(t, response); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d", got, len(body))
	}

	cached, ok := fixture.onDisk(t, debPath)
	if !ok {
		t.Fatal("the package was served but not cached, so every request would refetch it")
	}
	if cached != body {
		t.Errorf("cached copy = %q, want %q", cached, body)
	}
}

// Anything the published index does not promise is a plain 404, and must not
// turn the server into an open proxy for arbitrary paths.
func TestOnDemandRejectsPathOutsideCatalog(t *testing.T) {
	fixture := newOnDemandFixture(t, map[string]string{debPath: "x"}, nil)

	response := fixture.get(t, http.MethodGet, "/pool/main/e/evil/evil_1.0_amd64.deb", nil)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
	if n := fixture.mirror.requests.Load(); n != 0 {
		t.Errorf("the mirror was contacted %d times for a path outside the catalog", n)
	}
}

// The generated index, Release and the databases are not in the catalog, so
// they must keep 404ing rather than being looked up upstream.
func TestOnDemandLeavesRepositoryFilesAlone(t *testing.T) {
	fixture := newOnDemandFixture(t, map[string]string{debPath: "x"}, nil)

	for _, path := range []string{
		"/dists/tinyrepo/Release",
		"/dists/tinyrepo/main/binary-amd64/Packages",
		"/tinyrepo.db",
	} {
		t.Run(path, func(t *testing.T) {
			response := fixture.get(t, http.MethodGet, path, nil)
			if response.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", response.StatusCode)
			}
		})
	}

	if n := fixture.mirror.requests.Load(); n != 0 {
		t.Errorf("the mirror was contacted %d times for repository metadata", n)
	}
}

// A mirror that refuses must not leave the bad response on disk, nor be
// reported to the client as a success.
func TestOnDemandMirrorFailureIsNotCached(t *testing.T) {
	fixture := newOnDemandFixture(t, map[string]string{debPath: "body"}, nil)

	// Present in the catalog, absent from the mirror.
	missing := "pool/main/g/ghost/ghost_1.0_amd64.deb"
	view := fixture.od.cat.Arches[0]
	fixture.od.cat.add(view, "ghost", manifestEntry{BaseURL: fixture.mirror.url(), PoolPath: missing, Size: 4})

	response := fixture.get(t, http.MethodGet, "/"+missing, nil)
	if response.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", response.StatusCode)
	}
	if _, ok := fixture.onDisk(t, missing); ok {
		t.Error("a failed download was left on disk, where a later run would accept it as good")
	}
}

// A body that does not match the published SHA256 must never be kept: that is
// the whole reason the checksum is in the index.
func TestOnDemandDiscardsCorruptDownload(t *testing.T) {
	const body = "the nano package"
	fixture := newOnDemandFixture(t, map[string]string{debPath: body}, nil)

	// Corrupt the expectation, which is the same thing as the mirror lying.
	entry := fixture.od.cat.Files[debPath]
	entry.SHA256 = strings.Repeat("00", sha256.Size)
	fixture.od.cat.Files[debPath] = entry

	fixture.get(t, http.MethodGet, "/"+debPath, nil)

	if _, ok := fixture.onDisk(t, debPath); ok {
		t.Error("a package whose checksum did not match was cached anyway")
	}
}

// HEAD is a size probe. Answering it by downloading the package would mean a
// probe costs as much as an install.
func TestOnDemandHeadDoesNotDownload(t *testing.T) {
	const body = "the nano package"
	fixture := newOnDemandFixture(t, map[string]string{debPath: body}, nil)

	response := fixture.get(t, http.MethodHead, "/"+debPath, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d", got, len(body))
	}
	if n := fixture.mirror.requests.Load(); n != 0 {
		t.Errorf("HEAD caused %d mirror requests, want 0", n)
	}
	if _, ok := fixture.onDisk(t, debPath); ok {
		t.Error("HEAD downloaded the package")
	}
}

// A Range on a miss is answered with the whole file. Documented behaviour: the
// second request is a hit, and hits do ranges properly.
func TestOnDemandRangeOnMissServesFullBody(t *testing.T) {
	const body = "0123456789"
	fixture := newOnDemandFixture(t, map[string]string{debPath: body}, nil)

	response := fixture.get(t, http.MethodGet, "/"+debPath, map[string]string{"Range": "bytes=2-5"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a range on a miss", response.StatusCode)
	}
	if got := fixture.body(t, response); got != body {
		t.Errorf("body = %q, want the whole %q", got, body)
	}
}

// Once cached, the file is served by the normal path, which is where byte
// ranges and conditional requests come from.
func TestOnDemandSecondRequestIsAHit(t *testing.T) {
	const body = "0123456789"
	fixture := newOnDemandFixture(t, map[string]string{debPath: body}, nil)

	fixture.get(t, http.MethodGet, "/"+debPath, nil)
	before := fixture.mirror.requests.Load()

	response := fixture.get(t, http.MethodGet, "/"+debPath, map[string]string{"Range": "bytes=2-5"})
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 once the package is cached", response.StatusCode)
	}
	if got := fixture.body(t, response); got != "2345" {
		t.Errorf("body = %q, want %q", got, "2345")
	}
	if after := fixture.mirror.requests.Load(); after != before {
		t.Errorf("a cached package was fetched again (%d -> %d)", before, after)
	}
}

// Ten machines running "apt install" at once must produce one download, not ten.
func TestOnDemandDeduplicatesConcurrentRequests(t *testing.T) {
	const body = "the nano package"
	const clients = 8

	fixture := newOnDemandFixture(t, map[string]string{debPath: body}, nil)
	fixture.mirror.release = make(chan struct{})

	var wg sync.WaitGroup
	bodies := make([]string, clients)

	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()

			response, err := fixture.server.Client().Get(fixture.server.URL + "/" + debPath)
			if err != nil {
				return
			}
			defer response.Body.Close()

			data, _ := io.ReadAll(response.Body)
			bodies[i] = string(data)
		}()
	}

	// Let every client arrive before the single download is allowed to finish.
	time.Sleep(200 * time.Millisecond)
	close(fixture.mirror.release)
	wg.Wait()

	if n := fixture.mirror.requests.Load(); n != 1 {
		t.Errorf("%d clients caused %d mirror requests, want exactly 1", clients, n)
	}
	for i, got := range bodies {
		if got != body {
			t.Errorf("client %d got %q, want %q", i, got, body)
		}
	}
}

// The client that triggers a miss is not the reason to fetch; the next request
// is. Losing a nearly complete package because someone hit Ctrl+C is the worst
// possible outcome, so the disk copy has to finish regardless.
func TestStreamThroughSurvivesClientDisconnect(t *testing.T) {
	const body = "a package body long enough to matter"

	mirror := newFakeMirror(t, map[string]string{debPath: body})
	dest := filepath.Join(t.TempDir(), "nano.deb")

	sum := sha256.Sum256([]byte(body))
	want := fileCheck{Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}

	// A writer that fails immediately, which is what a hung-up client looks like.
	broken := &failingWriter{err: errors.New("client went away")}

	if _, err := streamThrough(context.Background(), mirror.url()+"/"+debPath, dest, want, broken); err != nil {
		t.Fatalf("streamThrough returned %v, want the download to complete anyway", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the package was not cached after the client disconnected: %v", err)
	}
	if string(data) != body {
		t.Errorf("cached copy = %q, want %q", data, body)
	}
}

type failingWriter struct{ err error }

func (f *failingWriter) Write(p []byte) (int, error) { return 0, f.err }

// A failed transfer must not leave a temporary file behind that looks like a
// package to the directory listing.
func TestStreamThroughLeavesNoTempFile(t *testing.T) {
	mirror := newFakeMirror(t, map[string]string{})
	dir := t.TempDir()
	dest := filepath.Join(dir, "nano.deb")

	if _, err := streamThrough(context.Background(), mirror.url()+"/missing", dest, fileCheck{}, io.Discard); err == nil {
		t.Fatal("streamThrough accepted a 404 from the mirror")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("left behind %q", entry.Name())
	}
}

func TestStreamThroughRejectsBadSize(t *testing.T) {
	const body = "0123456789"

	mirror := newFakeMirror(t, map[string]string{debPath: body})
	dir := t.TempDir()
	dest := filepath.Join(dir, "nano.deb")

	_, err := streamThrough(context.Background(), mirror.url()+"/"+debPath, dest, fileCheck{Size: 999}, io.Discard)
	if err == nil {
		t.Fatal("streamThrough accepted a body whose size did not match the index")
	}
	if fileExists(dest) {
		t.Error("a short download was moved into place")
	}
}

// The acquirer is the reason a package is downloaded once however many clients
// ask for it, so its handover rules are worth pinning down directly.
func TestAcquirerLeaderAndFollowers(t *testing.T) {
	var a acquirer

	leaderInflight, leader := a.begin("p")
	if !leader {
		t.Fatal("the first caller was not made leader")
	}

	follower, isLeader := a.begin("p")
	if isLeader {
		t.Fatal("a second caller was also made leader, so the package downloads twice")
	}
	if follower != leaderInflight {
		t.Fatal("the follower is waiting on a different acquisition than the leader is running")
	}

	wanted := errors.New("mirror refused")
	go a.finish("p", leaderInflight, wanted)

	if err := follower.wait(context.Background()); !errors.Is(err, wanted) {
		t.Errorf("the follower saw err = %v, want %v", err, wanted)
	}

	// Once finished, the next caller starts a fresh acquisition rather than
	// attaching to one that has already ended.
	if _, leader := a.begin("p"); !leader {
		t.Error("a caller arriving after the download finished was not made leader")
	}
}

func TestAcquirerWaitHonoursContext(t *testing.T) {
	var a acquirer

	in, _ := a.begin("p")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := in.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("wait returned %v, want context.Canceled", err)
	}
}

// Prefetching is best effort: it must never make a request handler wait.
func TestPrefetcherDoesNotBlockWhenFull(t *testing.T) {
	block := make(chan struct{})
	defer close(block)

	p := newPrefetcher(context.Background(), 1, func(ctx context.Context, entry manifestEntry) error {
		<-block
		return nil
	})
	defer p.stop(time.Second)

	entries := make([]manifestEntry, 5000)
	for i := range entries {
		entries[i] = manifestEntry{PoolPath: fmt.Sprintf("pool/p%d.deb", i)}
	}

	done := make(chan struct{})
	go func() {
		p.enqueue(entries)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue blocked, so a request handler would have blocked with it")
	}
}

func TestPrefetcherRunsEachEntryOnce(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}

	p := newPrefetcher(context.Background(), 2, func(ctx context.Context, entry manifestEntry) error {
		mu.Lock()
		counts[entry.PoolPath]++
		mu.Unlock()
		return nil
	})
	defer p.stop(2 * time.Second)

	entry := manifestEntry{PoolPath: "pool/main/n/nano.deb"}
	for range 5 {
		p.enqueue([]manifestEntry{entry})
	}

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return counts[entry.PoolPath] > 0
	})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if counts[entry.PoolPath] != 1 {
		t.Errorf("the same package was prefetched %d times, want 1", counts[entry.PoolPath])
	}
}

// Stopping must not wait on a download that could take minutes.
func TestPrefetcherStopIsPrompt(t *testing.T) {
	p := newPrefetcher(context.Background(), 2, func(ctx context.Context, entry manifestEntry) error {
		<-ctx.Done()
		return ctx.Err()
	})

	p.enqueue([]manifestEntry{{PoolPath: "a"}, {PoolPath: "b"}})
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	p.stop(2 * time.Second)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("stop took %v, want it to be prompt", elapsed)
	}
}

// The point of the background thread: after one miss, the packages that request
// implies are already there.
func TestOnDemandPrefetchesDependencies(t *testing.T) {
	bodies := map[string]string{
		"pool/main/a/a/a_1_amd64.deb": "package a",
		"pool/main/b/b/b_1_amd64.deb": "package b",
		"pool/main/c/c/c_1_amd64.deb": "package c",
	}
	deps := map[string][]string{
		"a": {"b"},
		"b": {"c"},
	}

	fixture := newOnDemandFixture(t, bodies, deps)

	response := fixture.get(t, http.MethodGet, "/pool/main/a/a/a_1_amd64.deb", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	fixture.body(t, response)

	// b is a direct dependency and c a transitive one: both should arrive
	// without anybody asking for them.
	for _, path := range []string{"pool/main/b/b/b_1_amd64.deb", "pool/main/c/c/c_1_amd64.deb"} {
		waitFor(t, 5*time.Second, func() bool {
			_, ok := fixture.onDisk(t, path)
			return ok
		})

		got, ok := fixture.onDisk(t, path)
		if !ok {
			t.Errorf("%s was never prefetched", path)
			continue
		}
		if got != bodies[path] {
			t.Errorf("%s = %q, want %q", path, got, bodies[path])
		}
	}
}

// waitFor polls until done reports true, or fails the test.
func waitFor(t *testing.T, timeout time.Duration, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition still false after %v", timeout)
}

// Without on-demand the handler must behave exactly as it did before.
func TestOnDemandDisabledStillReturns404(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(repoHandler{root: root})
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/" + debPath)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when on-demand mode is off", response.StatusCode)
	}
}

// A half-written download must never be listed or served as if it were a
// package: on demand, downloading and serving happen at the same time.
func TestWebHidesTempFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pool")

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	partial := "nano_7.2-1_amd64.deb" + TempSuffix
	if err := os.WriteFile(filepath.Join(dir, partial), []byte("half"), 0644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(repoHandler{root: root})
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/pool/" + partial)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("a half-written package was served with status %d", response.StatusCode)
	}

	listing, err := server.Client().Get(server.URL + "/pool/")
	if err != nil {
		t.Fatal(err)
	}
	defer listing.Body.Close()

	page, _ := io.ReadAll(listing.Body)
	if strings.Contains(string(page), TempSuffix) {
		t.Error("a half-written package appeared in the directory listing")
	}
}
