package tinyrepo

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// filepath.Join cleans "..", it does not reject it, so an upstream Filename can
// otherwise be written anywhere the process can reach.
func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := filepath.FromSlash("/tmp/repo")

	tests := []struct {
		name     string
		relative string
		wantErr  bool
	}{
		{"a normal pool path", "pool/main/n/nano/nano_7.2_amd64.deb", false},
		{"a flat pacman package", "nano-8.6-1-x86_64.pkg.tar.zst", false},
		{"climbing out", "../../../../etc/cron.d/x", true},
		{"climbing out from inside", "pool/../../../etc/passwd", true},
		{"absolute", "/etc/passwd", true},
		{"just dot dot", "..", true},
		{"empty", "", true},
		{"trailing escape", "pool/main/../../..", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeJoin(root, tc.relative)

			if (err != nil) != tc.wantErr {
				t.Fatalf("safeJoin(%q) err = %v, wantErr = %v", tc.relative, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !strings.HasPrefix(got, root+string(filepath.Separator)) {
				t.Errorf("safeJoin(%q) = %q, which is outside %q", tc.relative, got, root)
			}
		})
	}
}

// The same rule, reached through the type the downloaders actually use.
func TestManifestEntryLocalPathRejectsTraversal(t *testing.T) {
	root := t.TempDir()

	good := manifestEntry{PoolPath: "pool/main/n/nano/nano_7.2_amd64.deb"}
	if _, err := good.localPath(root); err != nil {
		t.Errorf("a normal pool path was rejected: %v", err)
	}

	evil := manifestEntry{PoolPath: "../../../../etc/cron.d/tinyrepo"}
	if _, err := evil.localPath(root); !errors.Is(err, errPathEscapes) {
		t.Errorf("localPath(%q) err = %v, want errPathEscapes", evil.PoolPath, err)
	}
}

// A manifest is a file on disk: it may have been hand-edited, or written by an
// older build that did not check.
func TestReadManifestDropsEscapingEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.tsv")

	contents := ManifestHeader +
		"http://mirror.test\tpool/main/n/nano/nano_7.2_amd64.deb\t100\tabc\n" +
		"http://mirror.test\t../../../../etc/cron.d/tinyrepo\t100\tdef\n"

	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := readManifest(path)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("read %d entries, want only the safe one", len(entries))
	}
	if entries[0].PoolPath != "pool/main/n/nano/nano_7.2_amd64.deb" {
		t.Errorf("kept %q", entries[0].PoolPath)
	}
}

// And on the way into the catalog, which the prefetcher writes from without any
// client being involved.
func TestCatalogRejectsEscapingPath(t *testing.T) {
	cat := newRepoCatalog()
	view := newArchCatalogView("amd64", newCatalog())

	cat.add(view, "evil", manifestEntry{PoolPath: "../../../../etc/cron.d/x"})
	cat.add(view, "nano", manifestEntry{PoolPath: "pool/main/n/nano/nano_7.2_amd64.deb"})

	if _, ok := cat.entry("../../../../etc/cron.d/x"); ok {
		t.Error("an escaping path made it into the catalog")
	}
	if _, ok := cat.entry("pool/main/n/nano/nano_7.2_amd64.deb"); !ok {
		t.Error("the safe path was dropped too")
	}
}

// endless is a body that never finishes, which is what a hostile mirror sends.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	return len(p), nil
}

func TestCopyCappedStopsAtLimit(t *testing.T) {
	tests := []struct {
		name    string
		src     io.Reader
		limit   int64
		want    int64
		wantErr bool
	}{
		{"under the limit", strings.NewReader("hello"), 100, 5, false},
		{"exactly the limit", strings.NewReader("hello"), 5, 5, false},
		{"one byte over", strings.NewReader("hello!"), 5, 6, true},
		{"endless source", endless{}, 1024, 1025, true},
		{"zero limit", strings.NewReader("x"), 0, 1, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buffer bytes.Buffer

			written, err := copyCapped(&buffer, tc.src, tc.limit)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if written != tc.want {
				t.Errorf("wrote %d bytes, want %d", written, tc.want)
			}
			if tc.wantErr && !errors.Is(err, errTooLarge) {
				t.Errorf("err = %v, want errTooLarge", err)
			}
		})
	}
}

// oversizedMirror announces a small file and then sends far more.
func oversizedMirror(t *testing.T, announce int, send int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length: chunked, so the lie is only visible as it arrives.
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("A"), 4096)
		for written := 0; written < send; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	return server
}

// The check has to happen on the way in. Comparing sizes after io.Copy means
// the disk is already full by the time the mismatch is noticed.
func TestDownloadRejectsOversizedBody(t *testing.T) {
	mirror := oversizedMirror(t, 10, 4<<20)
	dir := t.TempDir()
	dest := filepath.Join(dir, "nano.deb")

	err := downloadFile(mirror.URL+"/nano.deb", dest, fileCheck{Size: 10})
	if err == nil {
		t.Fatal("downloadFile accepted a body far larger than the index promised")
	}

	if fileExists(dest) {
		t.Error("the oversized body was left in place")
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		t.Errorf("left behind %q", entry.Name())
	}
}

func TestStreamThroughRejectsOversizedBody(t *testing.T) {
	mirror := oversizedMirror(t, 10, 4<<20)
	dir := t.TempDir()
	dest := filepath.Join(dir, "nano.deb")

	var client bytes.Buffer

	_, err := streamThrough(context.Background(), mirror.URL+"/nano.deb", dest,
		fileCheck{Size: 10}, &client)
	if err == nil {
		t.Fatal("streamThrough accepted a body far larger than the index promised")
	}

	if fileExists(dest) {
		t.Error("the oversized body was moved into place")
	}
	if int64(client.Len()) > 4<<20 {
		t.Errorf("relayed %d bytes to the client with a limit of 10", client.Len())
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		t.Errorf("left behind %q", entry.Name())
	}
}

// A small compressed file says nothing about how big it becomes.
func TestDecompressRejectsBomb(t *testing.T) {
	dir := t.TempDir()
	bomb := filepath.Join(dir, "Packages.gz")

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	chunk := bytes.Repeat([]byte("A"), 64<<10)
	for range 160 { // 10 MiB expanded, from a few KB on disk
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bomb, compressed.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	t.Logf("%d compressed bytes expanding to %d", compressed.Len(), 160*len(chunk))

	if err := decompressLimit(bomb, 1<<20); err == nil {
		t.Fatal("decompressLimit expanded past its limit")
	}
	if fileExists(filepath.Join(dir, "Packages")) {
		t.Error("the over-expanded file was left in place")
	}

	// And the same file within a limit that allows it must still work, so the
	// guard is not just refusing everything.
	if err := decompressLimit(bomb, 32<<20); err != nil {
		t.Errorf("a legitimate expansion was rejected: %v", err)
	}
}

// A predictable temporary name is a symlink waiting to happen.
func TestWriteFileAtomicDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Packages")

	bait := filepath.Join(t.TempDir(), "important")
	if err := os.WriteFile(bait, []byte("do not touch"), 0644); err != nil {
		t.Fatal(err)
	}

	// What an attacker who can write in the destination directory would do.
	if err := os.Symlink(bait, target+TempSuffix); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := writeFileAtomic(target, func(w io.Writer) error {
		_, err := io.WriteString(w, "written by tinyrepo")
		return err
	})
	if err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	contents, err := os.ReadFile(bait)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "do not touch" {
		t.Errorf("the symlink target was overwritten with %q", contents)
	}
}

func TestFetchLimitBlocksAndTimesOut(t *testing.T) {
	limit := newFetchLimit(2)
	ctx := context.Background()

	if !limit.acquire(ctx, time.Second) || !limit.acquire(ctx, time.Second) {
		t.Fatal("the first two slots were refused")
	}

	// Full: a caller willing to wait a moment still gets nothing.
	start := time.Now()
	if limit.acquire(ctx, 100*time.Millisecond) {
		t.Fatal("a third slot was handed out with a limit of two")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("gave up after %v, before the wait had elapsed", elapsed)
	}

	// Background work asks with no wait at all and must not block.
	start = time.Now()
	if limit.acquire(ctx, 0) {
		t.Error("a zero wait acquired a slot from a full limit")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("a zero wait blocked for %v", elapsed)
	}

	limit.release()
	if !limit.acquire(ctx, time.Second) {
		t.Error("a released slot was not reusable")
	}
}

func TestFetchLimitHonoursContext(t *testing.T) {
	limit := newFetchLimit(1)
	ctx, cancel := context.WithCancel(context.Background())

	if !limit.acquire(context.Background(), time.Second) {
		t.Fatal("the only slot was refused")
	}
	cancel()

	start := time.Now()
	if limit.acquire(ctx, 10*time.Second) {
		t.Error("a cancelled request was given a slot")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a cancelled request waited %v instead of returning at once", elapsed)
	}
}

// The whole point: one client asking for many different packages must not open
// one upstream download per package.
func TestOnDemandLimitsConcurrentFetches(t *testing.T) {
	const packages = 30
	const limit = 2

	release := make(chan struct{})
	var mu sync.Mutex
	inFlight, peak := 0, 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		<-release

		mu.Lock()
		inFlight--
		mu.Unlock()

		io.WriteString(w, "package")
	}))
	defer upstream.Close()

	root := t.TempDir()
	config := defaultConfig()
	config.Destination.Path = root
	config.Settings.MaxConcurrentDownloads = limit
	config.Settings.VerifyChecksum = false

	cat := newRepoCatalog()
	view := newArchCatalogView("amd64", newCatalog())
	var wanted []string

	for i := range packages {
		path := "pool/main/p/pkg" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".deb"
		view.Deps.addPackage(path, nil, nil, nil)
		cat.add(view, path, manifestEntry{BaseURL: upstream.URL, PoolPath: path, Size: 7})
		wanted = append(wanted, path)
	}
	cat.Arches = append(cat.Arches, view)

	od := &onDemand{
		config: &config, cat: cat, root: root,
		ctx:   context.Background(),
		limit: newFetchLimit(limit),
	}
	od.pre = newPrefetcher(context.Background(), 1, od.fetch)
	defer od.pre.stop(time.Second)

	server := httptest.NewServer(repoHandler{root: root, onDemand: od})
	defer server.Close()

	var wg sync.WaitGroup
	for _, path := range wanted {
		wg.Add(1)
		go func() {
			defer wg.Done()

			response, err := server.Client().Get(server.URL + "/" + path)
			if err != nil {
				return
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}()
	}

	// Let them pile up, then let the mirror answer.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	observed := peak
	mu.Unlock()

	close(release)
	wg.Wait()

	if observed > limit {
		t.Errorf("%d downloads ran at once with maxConcurrentDownloads = %d", observed, limit)
	}
	if observed == 0 {
		t.Error("no download reached the mirror at all")
	}
	t.Logf("%d concurrent requests produced at most %d upstream downloads", packages, observed)
}

// Past the limit a request is told to come back rather than held open until apt
// or pacman gives up on it.
func TestOnDemandReturns503WhenSaturated(t *testing.T) {
	release := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, "package")
	}))
	defer upstream.Close()

	root := t.TempDir()
	config := defaultConfig()
	config.Destination.Path = root
	config.Settings.VerifyChecksum = false

	cat := newRepoCatalog()
	view := newArchCatalogView("amd64", newCatalog())
	for _, name := range []string{"a", "b"} {
		path := "pool/" + name + ".deb"
		view.Deps.addPackage(path, nil, nil, nil)
		cat.add(view, path, manifestEntry{BaseURL: upstream.URL, PoolPath: path, Size: 7})
	}
	cat.Arches = append(cat.Arches, view)

	// A limit of one, so the second request has nowhere to go, and a short wait
	// so the test does not sit out the real 30 seconds.
	od := &onDemand{
		config: &config, cat: cat, root: root,
		ctx:   context.Background(),
		limit: newFetchLimit(1),
		wait:  200 * time.Millisecond,
	}
	od.pre = newPrefetcher(context.Background(), 1, od.fetch)
	defer od.pre.stop(time.Second)

	stats := newServerStats()
	od.stats = stats

	server := httptest.NewServer(repoHandler{root: root, onDemand: od, stats: stats})
	defer server.Close()
	// Before server.Close, which waits for the request still parked on it.
	defer close(release)

	// Occupy the only slot.
	go func() {
		response, err := server.Client().Get(server.URL + "/pool/a.deb")
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	response, err := server.Client().Get(server.URL + "/pool/b.deb")
	if err != nil {
		t.Fatalf("the second request was never answered: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 once the limit is saturated", response.StatusCode)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Error("the 503 carries no Retry-After, so a client has nothing to go on")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the refusal took %v; a saturated server has to shed load, not queue it", elapsed)
	}
	// Shedding load is exactly what somebody watching /healthz needs to see:
	// nothing else in the report distinguishes it from a quiet server.
	if got := stats.fetchBusy.Load(); got != 1 {
		t.Errorf("busy = %d after one refusal, want 1", got)
	}
}

// net/http accepts every connection it is handed, and each one is a goroutine
// and a file descriptor. A client opening slow connections must not be able to
// starve the process of descriptors.
func TestLimitListenerCapsConnections(t *testing.T) {
	const cap = 3

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newLimitListener(inner, cap)
	defer listener.Close()

	var mu sync.Mutex
	var accepted []net.Conn
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
		}
	}()

	// Open twice the cap. The extra ones sit in the kernel's accept queue.
	var dialed []net.Conn
	for range cap * 2 {
		conn, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		dialed = append(dialed, conn)
	}
	defer func() {
		for _, conn := range dialed {
			conn.Close()
		}
	}()

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	got := len(accepted)
	mu.Unlock()

	if got > cap {
		t.Fatalf("accepted %d connections at once, the cap is %d", got, cap)
	}
	if got != cap {
		t.Fatalf("accepted %d connections, want the cap of %d to be filled", got, cap)
	}

	// Closing one accepted connection has to free exactly one slot.
	mu.Lock()
	first := accepted[0]
	mu.Unlock()
	first.Close()

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	after := len(accepted)
	mu.Unlock()

	if after != cap+1 {
		t.Errorf("after freeing one slot %d connections had been accepted, want %d", after, cap+1)
	}
}

// net/http closes a connection from more than one place; releasing twice would
// raise the cap by one every time it happened.
func TestLimitConnReleasesSlotOnce(t *testing.T) {
	slots := make(chan struct{}, 2)
	slots <- struct{}{}

	server, client := net.Pipe()
	defer server.Close()

	conn := &limitConn{Conn: client, release: func() { <-slots }}

	conn.Close()
	conn.Close()
	conn.Close()

	if len(slots) != 0 {
		t.Fatalf("%d slots still held after closing", len(slots))
	}

	// A second holder must still be able to take a slot, and only one.
	slots <- struct{}{}
	slots <- struct{}{}
	if len(slots) != 2 {
		t.Errorf("the semaphore holds %d slots, want 2: a slot was released more than once", len(slots))
	}
}
