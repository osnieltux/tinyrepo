package tinyrepo

import (
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"
)

// httpClient is built once from the config so connections are reused across
// downloads. Building a Transport per request disables keep-alive entirely.
var httpClient = http.DefaultClient

// initHTTPClient configures the shared client, including the proxy if enabled.
func initHTTPClient(config *Config) error {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   config.Settings.MaxConcurrentDownloads,
	}

	if config.Proxy.Use {
		raw := config.Proxy.Host + ":" + strconv.Itoa(config.Proxy.Port)
		proxyURL, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("%s: %v", _t("invalid p"), err)
		}
		if proxyURL.Scheme == "" || proxyURL.Host == "" {
			return fmt.Errorf("%s: %q", _t("invalid p"), raw)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}

	// No Client.Timeout: it would abort long but healthy downloads. The
	// per-phase timeouts on the transport cover a stalled server.
	httpClient = &http.Client{Transport: transport}
	return nil
}

func logDebug(v ...any) {
	if DEBUG {
		log.Println(v...)
	}
}

func logDebugf(format string, v ...any) {
	if DEBUG {
		log.Printf(format, v...)
	}
}

// logError always reports, regardless of DEBUG. log writes to stderr.
func logError(v ...any) {
	log.Println(v...)
}

func logErrorf(format string, v ...any) {
	log.Printf(format, v...)
}

func fileExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

func recursiveFinder(root string, fileName string) ([]string, error) {
	var found []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && d.Name() == fileName {
			found = append(found, path)
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	slices.Sort(found) // deterministic order across runs
	return found, nil
}

// runConcurrent runs tasks with at most n running at the same time.
func runConcurrent(n int, tasks []func()) {
	if n < 1 {
		n = 1
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup

	for _, task := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(task func()) {
			defer wg.Done()
			defer func() { <-sem }()
			task()
		}(task)
	}
	wg.Wait()
}

// writeFileAtomic writes through a temporary file and renames it into place, so
// a failure never leaves a truncated file that later runs would accept as good.
func writeFileAtomic(path string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("%s %s: %v", _t("error c p"), filepath.Dir(path), err)
	}

	// A unique name rather than path+TempSuffix. A fixed one is predictable, so
	// anyone able to write in this directory can pre-create it as a symlink and
	// have the write land on whatever it points at; and two tinyrepo processes
	// writing the same file would interleave into one temporary and then both
	// rename it. CreateTemp is O_EXCL, so neither is possible.
	out, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*"+TempSuffix)
	if err != nil {
		return fmt.Errorf("%s: %v", _t("error c f"), err)
	}
	tmp := out.Name()

	if err := write(out); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}

	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// decompress expands Packages.xz or Packages.gz next to the source file, up to
// the default ceiling.
func decompress(file string) error {
	return decompressLimit(file, MaxIndexSize)
}

// decompressLimit is decompress with the expanded size bounded by the caller.
//
// A compressed file says nothing about how big it becomes: a one megabyte
// Packages.xz can expand to terabytes and fill the disk. When the upstream
// Release is available it states the exact uncompressed size, which is the
// limit worth passing here.
func decompressLimit(file string, limit int64) error {
	var dest string
	var newReader func(io.Reader) (io.ReadCloser, error)

	switch filepath.Ext(file) {
	case ".xz":
		dest = strings.TrimSuffix(file, ".xz")
		newReader = func(r io.Reader) (io.ReadCloser, error) {
			xzReader, err := xz.NewReader(r)
			if err != nil {
				return nil, err
			}
			return io.NopCloser(xzReader), nil
		}
	case ".gz":
		dest = strings.TrimSuffix(file, ".gz")
		newReader = func(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) }
	default:
		return fmt.Errorf("%s: %s", _t("unknown ext"), file)
	}

	in, err := os.Open(file)
	if err != nil {
		return err
	}
	defer in.Close()

	return writeFileAtomic(dest, func(w io.Writer) error {
		reader, err := newReader(in)
		if err != nil {
			return err
		}
		defer reader.Close()

		_, err = copyCapped(w, reader, limit)
		return err
	})
}

// compressFile writes path+".gz" and path+".xz" beside the given file.
func compressFile(path string) error {
	_, err := compressIndex(path, true)
	return err
}

// compressIndex writes path+".gz", and path+".xz" only when asked. It returns
// the base names it produced, so Release can describe exactly what exists
// rather than promising a variant that was skipped.
func compressIndex(path string, withXZ bool) ([]string, error) {
	base := filepath.Base(path)

	if err := writeGzip(path); err != nil {
		return nil, err
	}
	if !withXZ {
		return []string{base + ".gz"}, nil
	}
	if err := writeXZ(path); err != nil {
		return nil, err
	}
	return []string{base + ".gz", base + ".xz"}, nil
}

func writeGzip(path string) error {
	return writeFileAtomic(path+".gz", func(w io.Writer) error {
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()

		gzWriter := gzip.NewWriter(w)
		if _, err := io.Copy(gzWriter, in); err != nil {
			gzWriter.Close()
			return err
		}
		return gzWriter.Close()
	})
}

func writeXZ(path string) error {
	return writeFileAtomic(path+".xz", func(w io.Writer) error {
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()

		xzWriter, err := xz.NewWriter(w)
		if err != nil {
			return err
		}
		if _, err := io.Copy(xzWriter, in); err != nil {
			xzWriter.Close()
			return err
		}
		return xzWriter.Close()
	})
}

// hashFile returns the MD5 and SHA256 of a file, plus its size.
func hashFile(path string) (md5Hex string, sha256Hex string, size int64, err error) {
	in, err := os.Open(path)
	if err != nil {
		return "", "", 0, err
	}
	defer in.Close()

	md5Sum := md5.New()
	sha256Sum := sha256.New()

	size, err = io.Copy(io.MultiWriter(md5Sum, sha256Sum), in)
	if err != nil {
		return "", "", 0, err
	}

	return hex.EncodeToString(md5Sum.Sum(nil)), hex.EncodeToString(sha256Sum.Sum(nil)), size, nil
}

// fileCheck describes what a downloaded file must match. A zero value means
// nothing is known about the file, so only the size heuristic is available.
type fileCheck struct {
	SHA256 string
	Size   int64
}

// matchesLocal reports whether an already-present file satisfies the check.
func (c fileCheck) matchesLocal(path string) bool {
	if c.SHA256 == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if c.Size > 0 && info.Size() != c.Size {
		return false
	}
	_, sha256Hex, _, err := hashFile(path)
	if err != nil {
		return false
	}
	return strings.EqualFold(sha256Hex, c.SHA256)
}

func downloadFile(fileURL, outputPath string, want fileCheck) error {
	return downloadFileContext(context.Background(), fileURL, outputPath, want)
}

// downloadFileContext is downloadFile with a deadline the caller controls. The
// on-demand prefetch uses it so that stopping the web server does not leave
// background downloads running.
func downloadFileContext(ctx context.Context, fileURL, outputPath string, want fileCheck) error {
	// A known checksum is a stronger and cheaper test than an extra HEAD.
	if want.matchesLocal(outputPath) {
		logDebugf("%s: %v", _t("already d"), outputPath)
		return nil
	}

	if want.SHA256 == "" && SkipDownloadSameSize {
		same, err := remoteSizeMatches(fileURL, outputPath)
		if err != nil {
			return err
		}
		if same {
			logDebugf("%s: %v", _t("already d"), fileURL)
			return nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return fmt.Errorf("%s: %v", _t("error d"), err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %v", _t("error d"), err)
	}
	defer resp.Body.Close()

	// Without this check a 404 error page gets written to disk as if it were
	// the requested file.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", _t("unexpected status"), resp.Status, fileURL)
	}

	// Stop a runaway body as it arrives. Comparing sizes after io.Copy means
	// the disk is already full by the time the mismatch is noticed.
	limit := want.Size
	if limit <= 0 {
		limit = MaxIndexSize
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("%s: %s (%d > %d)", _t("size mismatch"), fileURL, resp.ContentLength, limit)
	}

	var sha256Sum hash.Hash

	err = writeFileAtomic(outputPath, func(w io.Writer) error {
		if want.SHA256 != "" {
			sha256Sum = sha256.New()
			w = io.MultiWriter(w, sha256Sum)
		}

		written, copyErr := copyCapped(w, resp.Body, limit)
		if copyErr != nil {
			return fmt.Errorf("%s: %v", _t("error s c"), copyErr)
		}
		if want.Size > 0 && written != want.Size {
			return fmt.Errorf("%s: %s (%d != %d)", _t("size mismatch"), fileURL, written, want.Size)
		}
		if sha256Sum != nil {
			got := hex.EncodeToString(sha256Sum.Sum(nil))
			if !strings.EqualFold(got, want.SHA256) {
				return fmt.Errorf("%s: %s", _t("checksum mismatch"), fileURL)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	logDebug(_t("downloaded"), outputPath)
	return nil
}

// remoteSizeMatches compares the local file size with the remote
// Content-Length. It is a weak check, used only when no checksum is known.
func remoteSizeMatches(fileURL, outputPath string) (bool, error) {
	info, err := os.Stat(outputPath)
	if err != nil {
		return false, nil // no local file, nothing to compare
	}

	headReq, err := http.NewRequest(http.MethodHead, fileURL, nil)
	if err != nil {
		return false, fmt.Errorf("%s: %v", _t("failed t c HEAD r"), err)
	}

	headResp, err := httpClient.Do(headReq)
	if err != nil {
		return false, fmt.Errorf("%s: %v", _t("failed t p HEAD r"), err)
	}
	defer headResp.Body.Close()

	if headResp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%s: %s", _t("unexpected HEAD r"), headResp.Status)
	}

	contentLength := headResp.Header.Get("Content-Length")
	if contentLength == "" {
		return false, nil
	}
	return contentLength == strconv.FormatInt(info.Size(), 10), nil
}

func setSystemLanguage() {
	// es_ES.UTF-8 => es; en_US.UTF-8 => en
	lang := ""

	if runtime.GOOS == "windows" {
		cmd := exec.Command("powershell", "Get-Culture | select -exp Name")
		output, err := cmd.Output()
		if err != nil {
			Language = "en"
			return
		}
		lang = strings.TrimSpace(string(output))
	} else {
		// POSIX precedence: LC_ALL beats LC_MESSAGES, which beats LANG.
		for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
			if value := os.Getenv(name); value != "" {
				lang = value
				break
			}
		}
	}

	Language = languageFromLocale(lang)
}

// languageFromLocale maps a locale string to a supported language code.
func languageFromLocale(locale string) string {
	if len(locale) < 2 {
		return "en"
	}
	code := strings.ToLower(locale[:2])
	if _, ok := messages[code]; ok {
		return code
	}
	return "en"
}

func _t(key string) string {
	if msg, ok := messages[Language][key]; ok {
		return msg
	}

	// Fallback to english
	if msg, ok := messages["en"][key]; ok {
		return msg
	}

	// return key if no translation found
	return key
}

func showErrorCodes() {
	codes := errorCodes[Language]
	if codes == nil {
		codes = errorCodes["en"]
	}

	for _, code := range slices.Sorted(maps.Keys(codes)) {
		fmt.Printf("%d %s\n", code, codes[code])
	}
}

func showDefaultHelp() {
	help, ok := DefaultHelp[Language]
	if !ok {
		help = DefaultHelp["en"]
	}
	fmt.Println(help)
}
