package main

import (
	"fmt"
	"net"
)

type Config struct {
	Server struct {
		// Type selects the repository format: "debian" or "arch".
		Type   string
		Source []string
	}
	Destination struct {
		Arch     []string
		Path     string
		Packages []string
	}
	Proxy struct {
		Use  bool
		Host string
		Port int
	}
	Web struct {
		// Listen is the address -ws binds to, as host:port.
		Listen string
		// BehindProxy says a reverse proxy terminates the connection, so
		// X-Forwarded-Proto decides the scheme the root page tells clients to
		// use. Off by default: the header is set by whoever is talking to us,
		// and if the port is reachable directly that is anybody.
		//
		// Not to be confused with [proxy], which is the outbound proxy used to
		// reach the mirror.
		BehindProxy bool
		// Health publishes /healthz, a JSON report of what the server is doing.
		// Off by default: it is a second thing the port answers, and one nobody
		// asked for is one nobody is watching either.
		Health bool
	}
	Settings struct {
		Debug                  bool
		SkipDownloadSameSize   bool
		MaxConcurrentDownloads int
		VerifyChecksum         bool
		// FilesDatabase generates <repo>.files so "pacman -F" works. It makes
		// the metadata download roughly ten times bigger. Ignored for Debian.
		FilesDatabase bool
		// OnDemand publishes the whole upstream catalog and lets -ws download a
		// package the moment a client asks for it. A client can only request
		// what it sees in the index, so publishing everything is what makes
		// downloading on demand possible at all.
		OnDemand bool
	}
}

func defaultConfig() Config {
	cfg := Config{}
	cfg.Server.Type = BackendDebian
	cfg.Settings.Debug = false
	// Keep these aligned with DefaultConfToml: otherwise a config file that
	// omits the field behaves differently from the documented example.
	cfg.Settings.SkipDownloadSameSize = true
	cfg.Settings.VerifyChecksum = true
	cfg.Settings.FilesDatabase = true
	// Off by default: on demand turns a small, predictable repository into one
	// that grows on its own, and that is the user's decision to make.
	cfg.Settings.OnDemand = false
	cfg.Settings.MaxConcurrentDownloads = DefaultMaxConcurrentDownloads
	cfg.Destination.Arch = []string{"amd64"}
	// Loopback by default: publishing a repository to every interface is the
	// user's decision to make, not a default to stumble into.
	cfg.Web.Listen = DefaultWebListen
	cfg.Web.BehindProxy = false
	cfg.Web.Health = false
	return cfg
}

func validateConfig(cfg *Config) error {
	if !knownBackend(cfg.Server.Type) {
		return fmt.Errorf("%s: [server].type, %s: %q (%s, %s)",
			_t("field"), _t("unknown type"), cfg.Server.Type, BackendDebian, BackendArch)
	}
	if len(cfg.Server.Source) == 0 {
		return fmt.Errorf("%s: [server].source, %s", _t("field"), _t("it i m a was n sp"))
	}
	if len(cfg.Destination.Path) == 0 {
		return fmt.Errorf("%s: [destination].path, %s", _t("field"), _t("it i m a was n sp"))
	}
	if len(cfg.Destination.Arch) == 0 {
		return fmt.Errorf("%s: [destination].arch, %s", _t("field"), _t("it i m a was n sp"))
	}
	for _, arch := range cfg.Destination.Arch {
		// Neither "all" (Debian) nor "any" (pacman) is a real index: those
		// packages live inside every concrete architecture's index and have
		// none of their own.
		if arch == "" || arch == "all" || arch == "any" {
			return fmt.Errorf("%s: [destination].arch, %s: %q", _t("field"), _t("invalid a"), arch)
		}
	}
	if cfg.Settings.MaxConcurrentDownloads < 1 || cfg.Settings.MaxConcurrentDownloads > MaxConcurrentDownloadsLimit {
		return fmt.Errorf("%s: [settings].maxConcurrentDownloads, %s (1-%d)",
			_t("field"), _t("out of range"), MaxConcurrentDownloadsLimit)
	}
	if _, _, err := net.SplitHostPort(cfg.Web.Listen); err != nil {
		return fmt.Errorf("%s: [web].listen, %s: %q (host:port)",
			_t("field"), _t("invalid listen"), cfg.Web.Listen)
	}
	if cfg.Proxy.Use {
		if cfg.Proxy.Host == "" {
			return fmt.Errorf("%s: [proxy].host, %s", _t("field"), _t("it i m a was n sp"))
		}
		if cfg.Proxy.Port < 1 || cfg.Proxy.Port > 65535 {
			return fmt.Errorf("%s: [proxy].port, %s (1-65535)", _t("field"), _t("out of range"))
		}
	}
	return nil
}
