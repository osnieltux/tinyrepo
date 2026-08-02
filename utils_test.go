package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestValidateConfig(t *testing.T) {
	valid := func() Config {
		cfg := defaultConfig()
		cfg.Server.Source = []string{"http://example.org/debian bookworm main"}
		cfg.Destination.Path = "/tmp/repo"
		return cfg
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(*Config) {}, false},
		{"no source", func(c *Config) { c.Server.Source = nil }, true},
		{"no path", func(c *Config) { c.Destination.Path = "" }, true},
		{"no arch", func(c *Config) { c.Destination.Arch = nil }, true},
		{"arch all", func(c *Config) { c.Destination.Arch = []string{"all"} }, true},
		{"empty arch", func(c *Config) { c.Destination.Arch = []string{""} }, true},
		{"concurrency zero", func(c *Config) { c.Settings.MaxConcurrentDownloads = 0 }, true},
		{"concurrency too high", func(c *Config) { c.Settings.MaxConcurrentDownloads = 999 }, true},
		{"proxy without host", func(c *Config) { c.Proxy.Use = true; c.Proxy.Port = 3128 }, true},
		{"proxy bad port", func(c *Config) { c.Proxy.Use = true; c.Proxy.Host = "http://h"; c.Proxy.Port = 0 }, true},
		{"proxy ok", func(c *Config) { c.Proxy.Use = true; c.Proxy.Host = "http://h"; c.Proxy.Port = 3128 }, false},
		{"on demand", func(c *Config) { c.Settings.OnDemand = true }, false},
		// On demand the declared list may legitimately be empty: everything
		// arrives because a client asked for it.
		{"on demand without packages", func(c *Config) { c.Settings.OnDemand = true; c.Destination.Packages = nil }, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(&cfg)

			if err := validateConfig(&cfg); (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// defaultConfig and the printed config.toml must agree, otherwise a config that
// omits a field behaves differently from the documented example.
func TestDefaultConfigMatchesSample(t *testing.T) {
	cfg := defaultConfig()

	for _, sample := range []struct {
		flag     string
		contents string
	}{
		{"-gc", DefaultConfTomlDebian},
		{"-ga", DefaultConfTomlArch},
	} {
		t.Run(sample.flag, func(t *testing.T) {
			if !cfg.Settings.SkipDownloadSameSize || !strings.Contains(sample.contents, "skipDownloadSameSize = true") {
				t.Error("skipDownloadSameSize disagrees with the sample")
			}
			if !cfg.Settings.VerifyChecksum || !strings.Contains(sample.contents, "verifyChecksum = true") {
				t.Error("verifyChecksum disagrees with the sample")
			}
			if cfg.Settings.MaxConcurrentDownloads != 4 || !strings.Contains(sample.contents, "maxConcurrentDownloads = 4") {
				t.Error("maxConcurrentDownloads disagrees with the sample")
			}
			if cfg.Web.Listen != DefaultWebListen || !strings.Contains(sample.contents, `listen = "`+DefaultWebListen+`"`) {
				t.Error("[web].listen disagrees with the sample")
			}
			if cfg.Settings.OnDemand || !strings.Contains(sample.contents, "onDemand = false") {
				t.Error("onDemand disagrees with the sample")
			}
			if cfg.Web.BehindProxy || !strings.Contains(sample.contents, "behindProxy = false") {
				t.Error("[web].behindProxy disagrees with the sample")
			}
			if cfg.Web.Health || !strings.Contains(sample.contents, "health = false") {
				t.Error("[web].health disagrees with the sample")
			}
		})
	}

	// filesDatabase only means anything to the pacman backend, so it is only
	// spelled out in that sample.
	if !cfg.Settings.FilesDatabase || !strings.Contains(DefaultConfTomlArch, "filesDatabase = true") {
		t.Error("filesDatabase disagrees with the Arch sample")
	}
}

// Each sample is what a user starts from, so it has to parse, validate, and
// select the backend it claims to be for.
func TestSampleConfigsAreUsable(t *testing.T) {
	tests := []struct {
		flag        string
		contents    string
		wantBackend string
		wantArch    string
	}{
		{"-gc", DefaultConfTomlDebian, BackendDebian, "amd64"},
		{"-ga", DefaultConfTomlArch, BackendArch, "x86_64"},
	}

	for _, tc := range tests {
		t.Run(tc.flag, func(t *testing.T) {
			cfg := defaultConfig()

			if _, err := toml.Decode(tc.contents, &cfg); err != nil {
				t.Fatalf("the config.toml printed by %s does not parse: %v", tc.flag, err)
			}
			if err := validateConfig(&cfg); err != nil {
				t.Fatalf("the config.toml printed by %s does not validate: %v", tc.flag, err)
			}

			if cfg.Server.Type != tc.wantBackend {
				t.Errorf("[server].type = %q, want %q", cfg.Server.Type, tc.wantBackend)
			}
			if _, err := newBackend(cfg.Server.Type); err != nil {
				t.Errorf("%s names an unknown backend: %v", tc.flag, err)
			}
			if len(cfg.Destination.Arch) == 0 || cfg.Destination.Arch[0] != tc.wantArch {
				t.Errorf("[destination].arch = %v, want %q first", cfg.Destination.Arch, tc.wantArch)
			}
			if len(cfg.Destination.Packages) == 0 {
				t.Error("[destination].packages is empty, so the sample builds nothing")
			}
			if len(cfg.Server.Source) == 0 {
				t.Error("[server].source is empty")
			}
		})
	}

	// The two samples must not be the same file with a different comment.
	if DefaultConfTomlDebian == DefaultConfTomlArch {
		t.Error("-gc and -ga print the same thing")
	}
}

// The help is written by hand, so nothing stops a new flag from going
// undocumented. This makes that a test failure.
func TestHelpDocumentsEveryFlag(t *testing.T) {
	set := flag.NewFlagSet("tinyrepo", flag.ContinueOnError)
	registerFlags(set)

	for language, help := range DefaultHelp {
		set.VisitAll(func(f *flag.Flag) {
			if !strings.Contains(help, "-"+f.Name) {
				t.Errorf("the %q help does not mention -%s", language, f.Name)
			}
		})
	}
}

// The old help ended with flag.PrintDefaults(), which lists the flags
// alphabetically with each description on its own indented line.
func TestHelpIsReadable(t *testing.T) {
	for language, help := range DefaultHelp {
		t.Run(language, func(t *testing.T) {
			// The build steps must be listed in the order they are run.
			di, ci := strings.Index(help, "-di"), strings.Index(help, "-ci")
			dp, ws := strings.Index(help, "-dp"), strings.Index(help, "-ws")

			if !(di < ci && ci < dp && dp < ws) {
				t.Errorf("the build steps are not listed in the order they are used: di=%d ci=%d dp=%d ws=%d",
					di, ci, dp, ws)
			}

			// Both package managers have to be told how to consume the result.
			for _, want := range []string{"deb [trusted=yes]", "SigLevel = Optional TrustAll"} {
				if !strings.Contains(help, want) {
					t.Errorf("the help does not show %q", want)
				}
			}

			for _, line := range strings.Split(help, "\n") {
				if strings.Contains(line, "\t") {
					t.Errorf("the help contains a tab, which lines up differently everywhere: %q", line)
				}
			}
		})
	}
}

func TestLanguageFromLocale(t *testing.T) {
	tests := []struct {
		locale string
		want   string
	}{
		{"es_ES.UTF-8", "es"},
		{"en_US.UTF-8", "en"},
		{"es-ES", "es"},
		{"ES_es", "es"},
		{"fr_FR.UTF-8", "en"}, // unsupported, falls back
		{"C", "en"},
		{"C.UTF-8", "en"},
		{"", "en"},
	}

	for _, tc := range tests {
		t.Run(tc.locale, func(t *testing.T) {
			if got := languageFromLocale(tc.locale); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTranslate(t *testing.T) {
	original := Language
	defer func() { Language = original }()

	Language = "es"
	if got := _t("downloaded"); got != "descargado:" {
		t.Errorf("es: got %q", got)
	}
	// A key missing in the active language falls back to english.
	Language = "en"
	if got := _t("downloaded"); got != "downloaded:" {
		t.Errorf("en: got %q", got)
	}
	// An unknown key returns itself instead of an empty string.
	if got := _t("no-such-key"); got != "no-such-key" {
		t.Errorf("unknown key: got %q", got)
	}
}

// Both language maps must define the same keys, otherwise one language
// silently falls back for some messages.
func TestMessagesAreComplete(t *testing.T) {
	for key := range messages["en"] {
		if _, ok := messages["es"][key]; !ok {
			t.Errorf("missing spanish translation for %q", key)
		}
	}
	for key := range messages["es"] {
		if _, ok := messages["en"][key]; !ok {
			t.Errorf("missing english translation for %q", key)
		}
	}
	for code := range errorCodes["en"] {
		if _, ok := errorCodes["es"][code]; !ok {
			t.Errorf("missing spanish text for exit code %d", code)
		}
	}
}

func TestCompressAndDecompressRoundTrip(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "Packages")
	content := strings.Repeat("Package: nano\nVersion: 7.2-1\n\n", 500)

	if err := os.WriteFile(source, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := compressFile(source); err != nil {
		t.Fatalf("compressFile: %v", err)
	}

	for _, extension := range []string{".gz", ".xz"} {
		t.Run(extension, func(t *testing.T) {
			compressed := source + extension
			if !fileExists(compressed) {
				t.Fatalf("%s was not created", compressed)
			}

			// Decompress into a directory of its own so it cannot overwrite
			// the original.
			target := filepath.Join(t.TempDir(), "Packages"+extension)
			data, err := os.ReadFile(compressed)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0644); err != nil {
				t.Fatal(err)
			}
			if err := decompress(target); err != nil {
				t.Fatalf("decompress: %v", err)
			}

			got := readFile(t, strings.TrimSuffix(target, extension))
			if got != content {
				t.Errorf("round trip changed the content (%d != %d bytes)", len(got), len(content))
			}
		})
	}
}

func TestDecompressRejectsUnknownExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Packages.bz2")

	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := decompress(path); err == nil {
		t.Error("expected an error for an unknown extension")
	}
}

// A corrupt archive must not leave a truncated output behind, because
// downloadIndex treats an existing Packages file as already decompressed.
func TestDecompressLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Packages.gz")

	if err := os.WriteFile(path, []byte("this is not gzip data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := decompress(path); err == nil {
		t.Fatal("expected an error for corrupt input")
	}
	if fileExists(filepath.Join(dir, "Packages")) {
		t.Error("a truncated Packages file was left behind")
	}
}

func sha256Of(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func TestDownloadFile(t *testing.T) {
	const body = "Package: nano\nVersion: 7.2-1\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			io.WriteString(w, body)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<html>404 Not Found</html>")
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	originalSkip := SkipDownloadSameSize
	SkipDownloadSameSize = false
	defer func() { SkipDownloadSameSize = originalSkip }()

	t.Run("success", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "pool", "Packages")

		if err := downloadFile(server.URL+"/ok", dest, fileCheck{}); err != nil {
			t.Fatalf("downloadFile: %v", err)
		}
		if got := readFile(t, dest); got != body {
			t.Errorf("got %q", got)
		}
	})

	// A 404 body used to be written to disk as if it were the real file.
	t.Run("404 is not written to disk", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "Packages")

		err := downloadFile(server.URL+"/missing", dest, fileCheck{})
		if err == nil {
			t.Fatal("expected an error for a 404 response")
		}
		if fileExists(dest) {
			t.Error("the error page was written to disk")
		}
	})

	t.Run("checksum is verified", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "nano.deb")

		want := fileCheck{SHA256: sha256Of(body), Size: int64(len(body))}
		if err := downloadFile(server.URL+"/ok", dest, want); err != nil {
			t.Fatalf("downloadFile: %v", err)
		}
	})

	t.Run("bad checksum leaves nothing behind", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "nano.deb")

		want := fileCheck{SHA256: sha256Of("something else")}
		if err := downloadFile(server.URL+"/ok", dest, want); err == nil {
			t.Fatal("expected a checksum mismatch")
		}
		if fileExists(dest) {
			t.Error("a file with the wrong checksum was kept")
		}
	})

	t.Run("size mismatch is rejected", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "nano.deb")

		want := fileCheck{SHA256: sha256Of(body), Size: 999999}
		if err := downloadFile(server.URL+"/ok", dest, want); err == nil {
			t.Fatal("expected a size mismatch")
		}
		if fileExists(dest) {
			t.Error("a file with the wrong size was kept")
		}
	})

	t.Run("a verified local file is not downloaded again", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "nano.deb")
		if err := os.WriteFile(dest, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}

		want := fileCheck{SHA256: sha256Of(body), Size: int64(len(body))}
		// An unreachable URL proves no request was made.
		if err := downloadFile("http://127.0.0.1:1/nope", dest, want); err != nil {
			t.Errorf("downloadFile: %v", err)
		}
	})
}

func TestWriteFileAtomicCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")

	err := writeFileAtomic(path, func(w io.Writer) error {
		io.WriteString(w, "partial")
		return io.ErrUnexpectedEOF
	})
	if err == nil {
		t.Fatal("expected the write error to propagate")
	}
	if fileExists(path) {
		t.Error("the destination was created despite the failure")
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary files were left behind: %v", entries)
	}
}

func TestRunConcurrent(t *testing.T) {
	const total = 100
	done := make([]bool, total)

	tasks := make([]func(), 0, total)
	for i := range total {
		tasks = append(tasks, func() { done[i] = true })
	}

	runConcurrent(4, tasks)

	for i, ok := range done {
		if !ok {
			t.Fatalf("task %d did not run", i)
		}
	}
}
