package tinyrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
)

// A pacman repository database is a gzipped tar holding one directory per
// package: "bash-5.3.15-1/desc", and in a .files database also
// "bash-5.3.15-1/files".

// archMember is one raw tar member, kept header and all so it can be written
// back out untouched.
type archMember struct {
	Header *tar.Header
	Data   []byte
}

// archEntry is one package of a pacman database: the fields needed to resolve
// and download it, plus the raw members it was read from.
//
// Keeping the members means the destination database is produced by copying
// them verbatim rather than by re-serialising desc, so no field can be lost and
// the %PGPSIG% signature survives.
type archEntry struct {
	Dir      string // "bash-5.3.15-1"
	Name     string
	Version  string
	Arch     string
	Filename string // bare file name: pacman repositories are flat
	Size     int64  // %CSIZE%, the size of the package file
	SHA256   string
	Depends  []string
	Provides []string
	Groups   []string
	Repo     string
	BaseURL  string
	Members  []archMember
}

// archDependencyName strips the version constraint from a pacman dependency.
//
//	"glibc>=2.34"          -> "glibc"
//	"libreadline.so=8-64"  -> "libreadline.so"
//	"python<3.12"          -> "python"
//
// Only the name is kept: the goal is to have the package present in the mirror,
// and matching exact versions is pacman's job at install time.
func archDependencyName(dep string) string {
	name := strings.TrimSpace(dep)

	// Every operator (=, >, <, >=, <=) starts with one of these three, so the
	// first occurrence is always the right place to cut.
	if i := strings.IndexAny(name, "<>="); i != -1 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

// archDependencyClauses converts a %DEPENDS% list into the clause form the
// shared resolver expects. pacman has no alternatives, so every clause holds
// exactly one name.
func archDependencyClauses(deps []string) [][]string {
	clauses := make([][]string, 0, len(deps))

	for _, dep := range deps {
		if name := archDependencyName(dep); name != "" {
			clauses = append(clauses, []string{name})
		}
	}
	return clauses
}

// archProvidedNames strips the versions from a %PROVIDES% list, so that
// "libreadline.so=8-64" can satisfy a dependency written the same way.
func archProvidedNames(provides []string) []string {
	names := make([]string, 0, len(provides))

	for _, entry := range provides {
		if name := archDependencyName(entry); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// isDescKey reports whether a line is a "%FIELD%" header rather than a value.
func isDescKey(line string) bool {
	if len(line) < 3 || line[0] != '%' || line[len(line)-1] != '%' {
		return false
	}
	for _, r := range line[1 : len(line)-1] {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// parseDesc reads the "%FIELD%" block format of a pacman desc file: a header
// line, then one value per line, until a blank line or the next header.
func parseDesc(data []byte) map[string][]string {
	fields := map[string][]string{}
	key := ""

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")

		switch {
		case isDescKey(line):
			key = line[1 : len(line)-1]
			if _, ok := fields[key]; !ok {
				fields[key] = nil
			}
		case line == "":
			key = ""
		case key != "":
			fields[key] = append(fields[key], line)
		}
	}
	return fields
}

func descFirst(fields map[string][]string, key string) string {
	if values := fields[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// readArchDB parses a pacman database file. It works for both <repo>.db and
// <repo>.files, since the latter is the same format with an extra member per
// package.
func readArchDB(dbPath, repo, baseURL string) ([]*archEntry, error) {
	file, err := os.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %v", _t("err r db"), dbPath, err)
	}
	defer gzReader.Close()

	byDir := map[string]*archEntry{}
	var order []string

	// Every member is decompressed into memory and kept, so that the database
	// can be rebuilt byte for byte. That makes an unbounded read a memory bomb:
	// the database itself carries no checksum, so a hostile mirror is free to
	// send a small gzip that expands without end.
	var total int64

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s %s: %v", _t("err r db"), dbPath, err)
		}

		dir := archMemberDir(header.Name)
		if dir == "" {
			continue
		}

		entry, ok := byDir[dir]
		if !ok {
			entry = &archEntry{Dir: dir, Repo: repo, BaseURL: baseURL}
			byDir[dir] = entry
			order = append(order, dir)
		}

		var data []byte
		if header.Typeflag == tar.TypeReg {
			total += header.Size
			if header.Size < 0 || total > MaxIndexSize {
				return nil, fmt.Errorf("%s %s: %v", _t("err r db"), dbPath, errTooLarge)
			}

			// Bounded by the header rather than read to exhaustion, and the
			// running total catches a database made of many small members.
			var buffer bytes.Buffer
			if _, err := copyCapped(&buffer, tarReader, header.Size); err != nil {
				return nil, fmt.Errorf("%s %s: %v", _t("err r db"), header.Name, err)
			}
			data = buffer.Bytes()
		}

		// Copy the header so the member can be written back byte for byte.
		copied := *header
		entry.Members = append(entry.Members, archMember{Header: &copied, Data: data})

		if path.Base(header.Name) == "desc" {
			applyDesc(entry, parseDesc(data))
		}
	}

	entries := make([]*archEntry, 0, len(order))
	for _, dir := range order {
		entry := byDir[dir]
		if entry.Name == "" {
			// A directory with no readable desc is not a package.
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func applyDesc(entry *archEntry, fields map[string][]string) {
	entry.Name = descFirst(fields, "NAME")
	entry.Version = descFirst(fields, "VERSION")
	entry.Arch = descFirst(fields, "ARCH")
	entry.Filename = descFirst(fields, "FILENAME")
	entry.SHA256 = descFirst(fields, "SHA256SUM")
	entry.Size, _ = strconv.ParseInt(descFirst(fields, "CSIZE"), 10, 64)

	// MAKEDEPENDS and CHECKDEPENDS are build-time only, and OPTDEPENDS is
	// optional by definition: none of them belongs in a runtime mirror.
	entry.Depends = fields["DEPENDS"]
	entry.Provides = fields["PROVIDES"]
	entry.Groups = fields["GROUPS"]
}

// archMemberDir returns the package directory a tar member belongs to.
func archMemberDir(name string) string {
	trimmed := strings.Trim(strings.TrimPrefix(name, "./"), "/")
	if trimmed == "" {
		return ""
	}

	if i := strings.Index(trimmed, "/"); i != -1 {
		return trimmed[:i]
	}
	return trimmed
}

// writeArchDB writes a pacman database holding the given packages' raw members.
func writeArchDB(destination string, entries []*archEntry) error {
	ordered := slices.Clone(entries)
	slices.SortFunc(ordered, func(a, b *archEntry) int { return strings.Compare(a.Dir, b.Dir) })

	var buffer bytes.Buffer

	gzWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzWriter)

	for _, entry := range ordered {
		members := slices.Clone(entry.Members)
		slices.SortFunc(members, func(a, b archMember) int {
			return strings.Compare(a.Header.Name, b.Header.Name)
		})

		for _, member := range members {
			header := *member.Header
			header.Size = int64(len(member.Data))

			if err := tarWriter.WriteHeader(&header); err != nil {
				return err
			}
			if _, err := tarWriter.Write(member.Data); err != nil {
				return err
			}
		}
	}

	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := gzWriter.Close(); err != nil {
		return err
	}

	// pacman asks for "<repo>.db" but the file is a gzipped tar; the name says
	// nothing about the format. Both names get the same bytes instead of a
	// symlink, so the repository also works over plain HTTP and on Windows.
	for _, name := range []string{destination, destination + ".tar.gz"} {
		err := writeFileAtomic(name, func(w io.Writer) error {
			_, err := w.Write(buffer.Bytes())
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
