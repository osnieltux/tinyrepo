package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/ulikunitz/xz"
)

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
	return found, nil
}

func decompress(file string) bool {
	// decompress Packages.xz or Package.gz
	// The output file will be the input file without the last 3 characters
	extension := string(file[len(file)-2:])
	dest := string(file[:len(file)-3])

	in, err := os.Open(file)
	if err != nil {
		return false
	}

	defer in.Close()

	out, err := os.Create(dest)
	if err != nil {
		return false
	}
	defer out.Close()

	if extension == "xz" {
		xzReader, err := xz.NewReader(in)
		if err != nil {
			return false
		}

		_, err = io.Copy(out, xzReader)
		if err == nil {
			return true
		} else {
			return false
		}

	}

	if extension == "gz" {

		gzReader, err := gzip.NewReader(in)
		if err != nil {
			return false
		}
		defer gzReader.Close()

		_, err = io.Copy(out, gzReader)
		if err == nil {
			return true
		} else {
			return false
		}
	}

	return false
}

func downloadFile(fileURL, outputPath string) error {
	var transport *http.Transport
	var localFileSize int64 = -1

	proxy, err := url.Parse(Proxy)
	if err != nil {
		return fmt.Errorf("%s: %v", _t("invalid p"), err)
	}

	if Proxy == "" {
		transport = &http.Transport{}
	} else {
		transport = &http.Transport{
			Proxy: http.ProxyURL(proxy),
		}
	}

	client := &http.Client{
		Transport: transport,
	}

	if fi, err := os.Stat(outputPath); err == nil {
		localFileSize = fi.Size()
		// lastModified = fi.ModTime().UTC()
	}

	if SkipDownloadSameSize {
		headReq, err := http.NewRequest("HEAD", fileURL, nil)
		if err != nil {
			return fmt.Errorf("%s: %v", _t("failed t c HEAD r"), err)
		}
		headResp, err := client.Do(headReq)
		if err != nil {
			return fmt.Errorf("%s: %v", _t("failed t p HEAD r"), err)
		}
		defer headResp.Body.Close()

		if headResp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", _t("unexpected HEAD r"), headResp.Status)
		}

		localFileSizeString := strconv.FormatInt(localFileSize, 10)

		if headResp.Header.Get("Content-Length") == localFileSizeString {
			if DEBUG {
				log.Printf("%s: %v", _t("already d"), fileURL)
			}
			return nil
		}
	}

	// make request
	resp, err := client.Get(fileURL)
	if err != nil {
		return fmt.Errorf("%s : %v",_t("error d"), err)
	}

	defer resp.Body.Close()

	/*contentType := resp.Header.Get("Content-Type")
	if contentType != "application/octet-stream" {
		return fmt.Errorf("requested URL is not downloadable: %v", fileURL)
	}*/

	dir := filepath.Dir(outputPath)
	errPath := os.MkdirAll(dir, 0755)
	if errPath != nil {
		if DEBUG {
			fmt.Println(_t("error c p"), ":", dir, errPath)
		}

		return errPath
	}

	// create file
	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("%s: %v",_t("error c f"), err)
	}
	defer out.Close()

	// write content to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %v",_t("error s c"), err)
	}

	return nil
}

func setSystemLanguage() {
	// es_ES.UTF-8 => es; en_US.UTF-8 => en
	lang := ""

	if runtime.GOOS == "windows" {
		cmd := exec.Command("powershell", "Get-Culture | select -exp Name")
		output, err := cmd.Output()
		if err == nil {
			lang = string(output)
		} else {
			Language = "en"
			return
		}

	} else {
		lang = os.Getenv("LANG")
		if lang == "" {
			lang = os.Getenv("LC_ALL")
		}
	}

	if lang == "" {
		Language = "en"
		return
	}

	if len(lang) > 2 {
		Language = lang[:2]
	}

	// check language is available and compatible
	for languageDict := range messages {
		// es; en
		if languageDict == Language {
			return
		}
	}

	// Language = strings.Split(lang, ".")[0][:2]
	
	// default language
	Language = "en"
}

func _t(key string) string {
	msg, ok := messages[Language][key]

	if ok {
		return msg
	}

	// Fallback to english
	msg, ok = messages["en"][key]

	if ok {
		return msg
	}
	
	// return key if no translation found
	return key
}

func showErrorCodes() {
	msg := errorCodes[Language]

	for p := range len(msg) {
		fmt.Println(p+1, errorCodes[Language][p+1])

	}
}

func showDefaultHelp() {
	fmt.Println(DefaultHelp[Language])
}
