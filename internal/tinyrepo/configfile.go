package tinyrepo

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// loadConfigFile reads and validates a config.toml. It does not touch the
// process-wide switches DEBUG, SkipDownloadSameSize and KeepRawStanzas: those
// belong to whoever started the process, and a panel-triggered action reloading
// the file must not change how the running web server behaves.
func loadConfigFile(path string) (*Config, error) {
	config := defaultConfig()

	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, fmt.Errorf("%s: %v", _t("config error"), err)
	}
	if err := validateConfig(&config); err != nil {
		return nil, fmt.Errorf("%s: %v", _t("config error"), err)
	}
	return &config, nil
}

// saveConfigFile writes a config back to disk, atomically, so a failed write
// never leaves a half-written config.toml that the next run would refuse.
func saveConfigFile(path string, config *Config) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	return writeFileAtomic(path, func(w io.Writer) error {
		return writeConfigTOML(w, config)
	})
}

// writeConfigTOML renders a config as the same commented file -gc prints,
// rather than as the bare key/value dump an encoder produces. The file stays
// something a person can open and read after the panel has written it.
func writeConfigTOML(w io.Writer, config *Config) error {
	var b strings.Builder

	b.WriteString("# tinyrepo - written by the web config panel\n")
	b.WriteString("# every field here is also documented in 'tinyrepo -gc'\n\n")

	b.WriteString("[server]\n")
	b.WriteString("# repository format: \"debian\" or \"arch\"\n")
	fmt.Fprintf(&b, "type = %s\n\n", quote(config.Server.Type))
	b.WriteString("# <mirror> <suite> <component...> for debian, <mirror> <repo...> for arch\n")
	writeList(&b, "source", config.Server.Source)

	b.WriteString("\n[destination]\n")
	writeList(&b, "arch", config.Destination.Arch)
	fmt.Fprintf(&b, "path = %s\n", quote(config.Destination.Path))
	b.WriteString("# the packages you want; their dependencies are pulled in automatically\n")
	writeList(&b, "packages", config.Destination.Packages)

	b.WriteString("\n[web]\n")
	b.WriteString("# address the -ws web server binds to\n")
	fmt.Fprintf(&b, "listen = %s\n", quote(config.Web.Listen))
	b.WriteString("# a reverse proxy in front terminates TLS, so trust X-Forwarded-Proto\n")
	fmt.Fprintf(&b, "behindProxy = %t\n", config.Web.BehindProxy)
	b.WriteString("# publish /healthz, a JSON report of what the server is doing\n")
	fmt.Fprintf(&b, "health = %t\n", config.Web.Health)
	b.WriteString("# publish /admin, this configuration panel. it requires a login\n")
	fmt.Fprintf(&b, "config = %t\n", config.Web.Config)
	b.WriteString("# the panel's account. the hash comes from 'tinyrepo -pw'\n")
	fmt.Fprintf(&b, "user = %s\n", quote(config.Web.User))
	fmt.Fprintf(&b, "passwordHash = %s\n", quote(config.Web.PasswordHash))

	b.WriteString("\n[proxy]\n")
	b.WriteString("# outbound proxy used to reach the mirror. not [web].behindProxy\n")
	fmt.Fprintf(&b, "use = %t\n", config.Proxy.Use)
	fmt.Fprintf(&b, "host = %s\n", quote(config.Proxy.Host))
	fmt.Fprintf(&b, "port = %d\n", config.Proxy.Port)

	b.WriteString("\n[settings]\n")
	fmt.Fprintf(&b, "debug = %t\n", config.Settings.Debug)
	b.WriteString("# verify the SHA256 published in the index after downloading each package\n")
	fmt.Fprintf(&b, "verifyChecksum = %t\n", config.Settings.VerifyChecksum)
	b.WriteString("# when no checksum is known, skip the download if the remote size matches\n")
	fmt.Fprintf(&b, "skipDownloadSameSize = %t\n", config.Settings.SkipDownloadSameSize)
	b.WriteString("# parallel downloads (1-32)\n")
	fmt.Fprintf(&b, "maxConcurrentDownloads = %d\n", config.Settings.MaxConcurrentDownloads)
	b.WriteString("# generate <repo>.files so \"pacman -F\" works. ignored for debian\n")
	fmt.Fprintf(&b, "filesDatabase = %t\n", config.Settings.FilesDatabase)
	b.WriteString("# publish the whole mirror catalog and download a package when asked for it\n")
	fmt.Fprintf(&b, "onDemand = %t\n", config.Settings.OnDemand)

	_, err := io.WriteString(w, b.String())
	return err
}

// quote renders a TOML basic string. strconv.Quote is Go syntax, which for
// everything TOML allows in a basic string is the same escaping.
func quote(s string) string {
	return strconv.Quote(s)
}

// writeList renders a string array, one entry per line so a long source list
// stays readable and diffs one line at a time.
func writeList(b *strings.Builder, name string, values []string) {
	if len(values) == 0 {
		fmt.Fprintf(b, "%s = []\n", name)
		return
	}
	if len(values) == 1 {
		fmt.Fprintf(b, "%s = [%s]\n", name, quote(values[0]))
		return
	}

	fmt.Fprintf(b, "%s = [\n", name)
	for _, value := range values {
		fmt.Fprintf(b, "  %s,\n", quote(value))
	}
	b.WriteString("]\n")
}

// parseLines turns a textarea into a list: one entry per line, blanks and
// surrounding space dropped. It is how every list field arrives from the panel.
func parseLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
