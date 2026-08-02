package main

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// webTestRepo lays out a directory that looks like a generated repository,
// including the internal directories the server must not publish.
func webTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	files := map[string]string{
		"dists/tinyrepo/Release":                       "Origin: Tinyrepo\n",
		"dists/tinyrepo/main/binary-amd64/Packages":    "Package: nano\n\n",
		"pool/main/n/nano/nano_7.2-1_amd64.deb":        "fake deb payload",
		"tinyrepo.db":                                  "fake pacman database",
		"lz4-1:1.10.0-2-x86_64.pkg.tar.zst":            "epoch in the file name",
		ManifestDirName + "/" + ManifestFileName:       "# secret-ish internal state\n",
		DistsCacheName + "/bookworm/main/Packages":     "cached upstream index\n",
		ArchCacheName + "/core/x86_64/core.db":         "cached upstream database\n",
		"dists/tinyrepo/main/binary-amd64/Packages.gz": "not really gzip",
	}

	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func webTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	root := webTestRepo(t)
	server := httptest.NewServer(repoHandler{root: root})
	t.Cleanup(server.Close)
	return server, root
}

// webGet performs a request without following redirects, so the redirect
// itself can be asserted on.
func webGet(t *testing.T, server *httptest.Server, method, path string, headers map[string]string) *http.Response {
	t.Helper()

	request, err := http.NewRequest(method, server.URL+path, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func webBody(t *testing.T, response *http.Response) string {
	t.Helper()

	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

func TestWebServesRepositoryFiles(t *testing.T) {
	server, _ := webTestServer(t)

	tests := []struct {
		path string
		want string
	}{
		{"/dists/tinyrepo/Release", "Origin: Tinyrepo\n"},
		{"/dists/tinyrepo/main/binary-amd64/Packages", "Package: nano\n\n"},
		{"/pool/main/n/nano/nano_7.2-1_amd64.deb", "fake deb payload"},
		{"/tinyrepo.db", "fake pacman database"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			response := webGet(t, server, http.MethodGet, tc.path, nil)

			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			if got := webBody(t, response); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

// apt and pacman both resume interrupted downloads with a byte range.
func TestWebSupportsByteRanges(t *testing.T) {
	server, _ := webTestServer(t)

	response := webGet(t, server, http.MethodGet, "/pool/main/n/nano/nano_7.2-1_amd64.deb",
		map[string]string{"Range": "bytes=5-8"})

	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", response.StatusCode)
	}
	if got := webBody(t, response); got != "deb " {
		t.Errorf("body = %q, want %q", got, "deb ")
	}
	if got := response.Header.Get("Content-Range"); got != "bytes 5-8/16" {
		t.Errorf("Content-Range = %q", got)
	}
}

func TestWebSupportsConditionalRequests(t *testing.T) {
	server, _ := webTestServer(t)

	first := webGet(t, server, http.MethodGet, "/dists/tinyrepo/Release", nil)
	modified := first.Header.Get("Last-Modified")
	if modified == "" {
		t.Fatal("no Last-Modified header, apt could not revalidate its cache")
	}

	second := webGet(t, server, http.MethodGet, "/dists/tinyrepo/Release",
		map[string]string{"If-Modified-Since": modified})

	if second.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304", second.StatusCode)
	}
}

func TestWebHeadReturnsHeadersWithoutBody(t *testing.T) {
	server, _ := webTestServer(t)

	response := webGet(t, server, http.MethodHead, "/pool/main/n/nano/nano_7.2-1_amd64.deb", nil)

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Length"); got != "16" {
		t.Errorf("Content-Length = %q, want 16", got)
	}
	if body := webBody(t, response); body != "" {
		t.Errorf("HEAD returned a body: %q", body)
	}
}

func TestWebRejectsOtherMethods(t *testing.T) {
	server, _ := webTestServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			response := webGet(t, server, method, "/dists/tinyrepo/Release", nil)

			if response.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", response.StatusCode)
			}
			if got := response.Header.Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow = %q", got)
			}
		})
	}
}

// The repository is read-only public data, but the server must still never
// hand out a file from outside the destination directory.
func TestWebRefusesPathTraversal(t *testing.T) {
	server, root := webTestServer(t)

	secret := filepath.Join(filepath.Dir(root), "outside-secret.txt")
	if err := os.WriteFile(secret, []byte("must never be served"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secret) })

	attempts := []string{
		"/../outside-secret.txt",
		"/../../outside-secret.txt",
		"/dists/../../outside-secret.txt",
		"/%2e%2e/outside-secret.txt",
		"/..%2foutside-secret.txt",
		"/dists/tinyrepo/../../../outside-secret.txt",
	}

	for _, attempt := range attempts {
		t.Run(attempt, func(t *testing.T) {
			response := webGet(t, server, http.MethodGet, attempt, nil)

			if body := webBody(t, response); strings.Contains(body, "must never be served") {
				t.Fatalf("%s served a file from outside the repository", attempt)
			}
			if response.StatusCode == http.StatusOK {
				t.Errorf("status = 200 for %s", attempt)
			}
		})
	}
}

// The upstream caches and tinyrepo's own state sit inside the destination
// directory but are not part of the published repository.
func TestWebHidesInternalDirectories(t *testing.T) {
	server, _ := webTestServer(t)

	hidden := []string{
		"/" + ManifestDirName + "/" + ManifestFileName,
		"/" + ManifestDirName + "/",
		"/" + DistsCacheName + "/bookworm/main/Packages",
		"/" + DistsCacheName + "/",
		"/" + ArchCacheName + "/core/x86_64/core.db",
	}

	for _, path := range hidden {
		t.Run(path, func(t *testing.T) {
			response := webGet(t, server, http.MethodGet, path, nil)

			if response.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", response.StatusCode)
			}
		})
	}

	// And they must not be advertised in the root listing either.
	body := webBody(t, webGet(t, server, http.MethodGet, "/", nil))
	for _, name := range []string{ManifestDirName, DistsCacheName, ArchCacheName} {
		if strings.Contains(body, name) {
			t.Errorf("the root listing mentions %q", name)
		}
	}
}

func TestWebDirectoryListing(t *testing.T) {
	server, _ := webTestServer(t)

	response := webGet(t, server, http.MethodGet, "/dists/tinyrepo/", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q", got)
	}

	body := webBody(t, response)

	for _, want := range []string{
		"<title>Index of /dists/tinyrepo/</title>",
		"<h1>Index of /dists/tinyrepo/</h1>",
		`<a href="../">../</a>`,
		`<a href="./main/">main/</a>`,
		`<a href="./Release">Release</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the listing is missing %q\n---\n%s", want, body)
		}
	}

	// Classic HTML only: nothing that needs a stylesheet or a script.
	for _, unwanted := range []string{"<style", "stylesheet", "<script", "class="} {
		if strings.Contains(strings.ToLower(body), unwanted) {
			t.Errorf("the listing uses %q, but it must be plain HTML", unwanted)
		}
	}
}

// Directories sort before files so the listing reads like a classic index.
func TestWebListingPutsDirectoriesFirst(t *testing.T) {
	server, _ := webTestServer(t)

	body := webBody(t, webGet(t, server, http.MethodGet, "/", nil))

	dists := strings.Index(body, `href="./dists/"`)
	db := strings.Index(body, `href="./tinyrepo.db"`)

	if dists == -1 || db == -1 {
		t.Fatalf("the root listing is missing entries\n%s", body)
	}
	if dists > db {
		t.Error("files are listed before directories")
	}
}

// A pacman package carrying an epoch has a colon in its name, which a browser
// reads as a URL scheme unless the link is relative to "./".
func TestWebLinksEpochFilenamesRelatively(t *testing.T) {
	server, _ := webTestServer(t)

	body := webBody(t, webGet(t, server, http.MethodGet, "/", nil))

	if !strings.Contains(body, `href="./lz4-1:1.10.0-2-x86_64.pkg.tar.zst"`) {
		t.Errorf("the epoch link is not relative to ./\n%s", body)
	}

	// And it must actually resolve.
	response := webGet(t, server, http.MethodGet, "/lz4-1:1.10.0-2-x86_64.pkg.tar.zst", nil)
	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", response.StatusCode)
	}
}

func TestWebRedirectsDirectoriesWithoutSlash(t *testing.T) {
	server, _ := webTestServer(t)

	response := webGet(t, server, http.MethodGet, "/dists", nil)

	if response.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", response.StatusCode)
	}
	if got := response.Header.Get("Location"); got != "/dists/" {
		t.Errorf("Location = %q, want /dists/", got)
	}
}

func TestWebRootShowsClientConfiguration(t *testing.T) {
	server, root := webTestServer(t)

	body := webBody(t, webGet(t, server, http.MethodGet, "/", nil))

	// The fixture holds both a Debian Release and a pacman database.
	if !strings.Contains(body, "deb [trusted=yes] http://") {
		t.Errorf("the root page does not show the sources.list line\n%s", body)
	}
	if !strings.Contains(body, "SigLevel = Optional TrustAll") {
		t.Errorf("the root page does not show the pacman.conf block\n%s", body)
	}

	// With no repository generated there is nothing to advertise.
	if err := os.Remove(filepath.Join(root, "tinyrepo.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "dists")); err != nil {
		t.Fatal(err)
	}

	body = webBody(t, webGet(t, server, http.MethodGet, "/", nil))
	if strings.Contains(body, "trusted=yes") || strings.Contains(body, "SigLevel") {
		t.Error("the root page advertises a repository that is not there")
	}
}

func TestWebMissingFileIsNotFound(t *testing.T) {
	server, _ := webTestServer(t)

	response := webGet(t, server, http.MethodGet, "/pool/main/n/nano/does-not-exist.deb", nil)

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}

func TestCleanRequestPath(t *testing.T) {
	tests := []struct {
		path   string
		want   string
		wantOK bool
	}{
		{"/", "/", true},
		{"/dists/tinyrepo/Release", "/dists/tinyrepo/Release", true},
		{"dists/", "/dists", true},
		{"/dists//tinyrepo/", "/dists/tinyrepo", true},
		{"/dists/./tinyrepo", "/dists/tinyrepo", true},
		{"/../etc/passwd", "/etc/passwd", true}, // clamped to the root, not escaped
		{"/dists/../../etc/passwd", "/etc/passwd", true},
		{"/" + ManifestDirName + "/manifest.tsv", "", false},
		{"/" + DistsCacheName, "", false},
		{"/" + ArchCacheName + "/core", "", false},
		{"/.hidden", "", false},
		{"/dists/.git/config", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := cleanRequestPath(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		size int64
		want string
	}{
		{0, "0"},
		{512, "512"},
		{1023, "1023"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{1024 * 1024, "1.0M"},
		{2002014, "1.9M"},
		{1024 * 1024 * 1024, "1.0G"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := humanSize(tc.size); got != tc.want {
				t.Errorf("humanSize(%d) = %q, want %q", tc.size, got, tc.want)
			}
		})
	}
}

func TestWebDisplayURL(t *testing.T) {
	tests := []struct {
		listen string
		want   string
	}{
		{"127.0.0.1:8080", "http://127.0.0.1:8080/"},
		{"192.168.1.10:8080", "http://192.168.1.10:8080/"},
		// A wildcard bind reports itself as "[::]" or "0.0.0.0", neither of
		// which is a host anyone can open.
		{"0.0.0.0:8080", "http://localhost:8080/"},
		{"[::]:8080", "http://localhost:8080/"},
	}

	for _, tc := range tests {
		t.Run(tc.listen, func(t *testing.T) {
			if got := webDisplayURL(fakeAddr(tc.listen)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeAddr is a net.Addr with a literal string, so the wildcard cases can be
// tested without actually binding to every interface.
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

func TestValidateConfigListenAddress(t *testing.T) {
	tests := []struct {
		listen  string
		wantErr bool
	}{
		{"127.0.0.1:8080", false},
		{"0.0.0.0:8080", false},
		{":8080", false},
		{"[::1]:8080", false},
		{"", true},
		{"8080", true},
		{"127.0.0.1", true},
	}

	for _, tc := range tests {
		t.Run(tc.listen, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Server.Source = []string{"http://mirror.example/debian bookworm main"}
			cfg.Destination.Path = "/tmp/repo"
			cfg.Web.Listen = tc.listen

			if err := validateConfig(&cfg); (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestServeRepoRejectsMissingRepository(t *testing.T) {
	cfg := defaultConfig()
	cfg.Destination.Path = filepath.Join(t.TempDir(), "not-generated-yet")
	cfg.Web.Listen = "127.0.0.1:0"

	if err := serveRepo(&cfg, debianBackend{}); err == nil {
		t.Error("serveRepo started with no repository to serve")
	}
}

// Behind a proxy that terminates TLS, the connection tinyrepo sees is plain
// http. If it believed only that, the home page would hand out an "http://"
// address for a repository published over https.
func TestRequestScheme(t *testing.T) {
	tests := []struct {
		name        string
		behindProxy bool
		forwarded   string
		tls         bool
		want        string
	}{
		{"plain, no proxy", false, "", false, "http"},
		{"direct TLS", false, "", true, "https"},
		{"behind a proxy terminating TLS", true, "https", false, "https"},
		{"behind a proxy without TLS", true, "http", false, "http"},
		{"a chain of proxies, the first faced the client", true, "https, http", false, "https"},
		{"upper case is still a scheme", true, "HTTPS", false, "https"},
		{"padded", true, "  https  ", false, "https"},

		// Not trusted: the header is set by whoever is talking to us.
		{"header ignored when not behind a proxy", false, "https", false, "http"},
		{"header cannot downgrade a direct TLS connection", false, "http", true, "https"},

		// Nonsense falls back to what the connection itself says.
		{"garbage scheme", true, "javascript:alert(1)", false, "http"},
		{"empty header", true, "", false, "http"},
		{"garbage over TLS", true, "gopher", true, "https"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.forwarded != "" {
				request.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			if tc.tls {
				request.TLS = &tls.ConnectionState{}
			}

			if got := requestScheme(request, tc.behindProxy); got != tc.want {
				t.Errorf("requestScheme = %q, want %q", got, tc.want)
			}
		})
	}
}

// The snippet on the home page is what a visitor pastes into sources.list, so
// it has to name the address they actually reached us on.
func TestClientHintsHonourForwardedScheme(t *testing.T) {
	root := webTestRepo(t)

	tests := []struct {
		name        string
		behindProxy bool
		want        string
	}{
		{"behind a TLS proxy", true, "deb [trusted=yes] https://repo.example.com/ tinyrepo main"},
		{"reachable directly", false, "deb [trusted=yes] http://repo.example.com/ tinyrepo main"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(repoHandler{root: root, behindProxy: tc.behindProxy})
			defer server.Close()

			// Host is set on the request itself, not as a header: that is what
			// nginx's "proxy_set_header Host $host" ends up producing, and it
			// is what clientHints reads.
			request, err := http.NewRequest(http.MethodGet, server.URL+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = "repo.example.com"
			request.Header.Set("X-Forwarded-Proto", "https")

			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()

			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(string(body), tc.want) {
				t.Errorf("the home page does not offer %q", tc.want)
			}
		})
	}
}
