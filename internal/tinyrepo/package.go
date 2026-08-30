package tinyrepo

// PackageDeb is one stanza of a Debian "Packages" index.
//
// Arch holds the package's own "Architecture:" field (which may be "all"),
// while IndexArch holds the architecture of the index it was read from
// (derived from the "binary-<arch>" directory name). They differ for
// "Architecture: all" packages, which appear in every architecture's index.
type PackageDeb struct {
	Package       string
	Version       string
	InstalledSize string
	Maintainer    string
	Arch          string
	IndexArch     string
	Size          string
	Filename      string
	FilenameUrl   string // base URL of the mirror this package came from
	MD5sum        string
	SHA256        string
	Pre_Depends   string
	Provides      string
	Breaks        string
	Description   string
	Tag           string
	Section       string
	Priority      string
	Homepage      string
	Depends       string

	// Raw is the stanza exactly as upstream wrote it, blank line included. It
	// is only captured when KeepRawStanzas is set, which on-demand mode does:
	// there the published index is the whole archive, and apt acts on fields
	// the resolver never reads - Conflicts, Replaces, Multi-Arch - so
	// re-serialising from the fields above would hand apt an archive whose
	// conflict graph had been deleted.
	Raw string
}
