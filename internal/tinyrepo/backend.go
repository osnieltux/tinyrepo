package tinyrepo

import "fmt"

// Repository formats tinyrepo can mirror.
const (
	BackendDebian = "debian"
	BackendArch   = "arch"
)

// Backend is everything that differs between repository formats.
//
// It is deliberately small: -dp is not part of it, because downloading the
// packages only needs the manifest that Build writes. That manifest - base URL,
// relative path, size, SHA256 - is the whole contract between the two halves,
// so the download, verification, retry and concurrency logic is shared.
type Backend interface {
	// FetchIndexes downloads the upstream metadata into the local cache (-di).
	FetchIndexes(config *Config) error

	// Build resolves the requested packages, writes the destination repository
	// and records what -dp has to download (-ci).
	Build(config *Config) error

	// Catalog reads the full upstream catalog from the local cache. It is what
	// on-demand serving looks a missing package up in, and what -cl resolves
	// the declared set against.
	Catalog(config *Config) (*repoCatalog, error)
}

// buildError lets a backend pick the documented exit code for a failure
// without the CLI having to know anything about the format.
type buildError struct {
	Code int
	Err  error
}

func (e *buildError) Error() string { return e.Err.Error() }
func (e *buildError) Unwrap() error { return e.Err }

func failBuild(code int, format string, args ...any) error {
	return &buildError{Code: code, Err: fmt.Errorf(format, args...)}
}

func knownBackend(name string) bool {
	switch name {
	case BackendDebian, BackendArch:
		return true
	}
	return false
}

func newBackend(name string) (Backend, error) {
	switch name {
	case BackendDebian:
		return debianBackend{}, nil
	case BackendArch:
		return archBackend{}, nil
	}
	return nil, fmt.Errorf("%s: %q", _t("unknown type"), name)
}

// debianBackend wires the existing Debian implementation behind the interface.
type debianBackend struct{}

func (debianBackend) FetchIndexes(config *Config) error {
	return downloadIndex(config)
}

func (debianBackend) Build(config *Config) error {
	published, declared, err := createDists(config)
	if err != nil {
		if err == errNoPackagesSelected {
			return &buildError{Code: 4, Err: err}
		}
		return &buildError{Code: 3, Err: err}
	}

	dists, err := generateDists(config, published)
	if err != nil {
		return &buildError{Code: 6, Err: err}
	}

	if err := generateRelease(dists); err != nil {
		return &buildError{Code: 13, Err: err}
	}

	// The manifest is what -dp downloads, so it stays the declared closure even
	// when the index published above advertises the whole mirror.
	if err := writeManifest(config, declared); err != nil {
		return &buildError{Code: 14, Err: err}
	}
	return nil
}
