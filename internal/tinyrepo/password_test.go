package tinyrepo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findPasswordField is the whole of -sp's decision: where the hash may be
// written, and why it may not be.

func TestFindPasswordFieldLocatesTheLiveField(t *testing.T) {
	lines := strings.Split(`[server]
type = "debian"

[web]
listen = "127.0.0.1:8080"
config = true
user = "admin"
passwordHash = ""
`, "\n")

	site, err := findPasswordField(lines)
	if err != nil {
		t.Fatalf("the field was not found: %v", err)
	}
	if site.Line != 8 {
		t.Errorf("line = %d, want 8", site.Line)
	}
	if site.Commented {
		t.Error("a live field was reported as commented")
	}
}

// The whole point of the command: a commented field is refused, and the message
// has to name the line so it can be found without hunting.
func TestFindPasswordFieldRefusesACommentedField(t *testing.T) {
	lines := strings.Split(`[web]
listen = "127.0.0.1:8080"
# config = true
# user = "admin"
# passwordHash = "pbkdf2-sha256$600000$...$..."
`, "\n")

	_, err := findPasswordField(lines)
	if !errors.Is(err, errFieldCommented) {
		t.Fatalf("err = %v, want errFieldCommented", err)
	}
	if !strings.Contains(err.Error(), "5") {
		t.Errorf("the error does not name line 5: %v", err)
	}
}

// Whatever the comment marker looks like, it is still a comment.
func TestFindPasswordFieldRecognisesEveryCommentStyle(t *testing.T) {
	for name, line := range map[string]string{
		"tight":      `#passwordHash = "x"`,
		"spaced":     `#   passwordHash = "x"`,
		"indented":   `  # passwordHash = "x"`,
		"tabbed":     "\t#\tpasswordHash = \"x\"",
		"doubled up": `## passwordHash = "x"`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := findPasswordField([]string{"[web]", line})
			if !errors.Is(err, errFieldCommented) {
				t.Errorf("%q: err = %v, want errFieldCommented", line, err)
			}
		})
	}
}

// A live field wins over a commented example, which is the normal shape of a
// config that has been edited from the sample.
func TestFindPasswordFieldPrefersTheLiveFieldOverAComment(t *testing.T) {
	lines := []string{
		"[web]",
		`# passwordHash = "pbkdf2-sha256$600000$...$..."`,
		`passwordHash = "already set"`,
	}

	site, err := findPasswordField(lines)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if site.Line != 3 || site.Commented {
		t.Errorf("found line %d (commented=%v), want the live line 3", site.Line, site.Commented)
	}
}

// The field only means anything under [web]; the same key elsewhere is a
// different setting and writing to it would do nothing.
func TestFindPasswordFieldOnlyCountsInsideWeb(t *testing.T) {
	lines := []string{
		"[settings]",
		`passwordHash = "wrong table"`,
	}

	_, err := findPasswordField(lines)
	if !errors.Is(err, errFieldMissing) {
		t.Errorf("err = %v, want errFieldMissing for a field outside [web]", err)
	}

	// And a field after [web] but under a later table does not count either.
	lines = []string{
		"[web]",
		"listen = \"127.0.0.1:8080\"",
		"[settings]",
		`passwordHash = "wrong table"`,
	}
	if _, err := findPasswordField(lines); !errors.Is(err, errFieldMissing) {
		t.Errorf("err = %v, want errFieldMissing once another table has opened", err)
	}
}

func TestFindPasswordFieldRefusesADuplicate(t *testing.T) {
	lines := []string{
		"[web]",
		`passwordHash = "one"`,
		`passwordHash = "two"`,
	}

	_, err := findPasswordField(lines)
	if !errors.Is(err, errFieldRepeated) {
		t.Fatalf("err = %v, want errFieldRepeated", err)
	}
	if !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "3") {
		t.Errorf("the error does not name both lines: %v", err)
	}
}

func TestFindPasswordFieldReportsAMissingField(t *testing.T) {
	lines := []string{"[web]", `listen = "127.0.0.1:8080"`}

	if _, err := findPasswordField(lines); !errors.Is(err, errFieldMissing) {
		t.Errorf("err = %v, want errFieldMissing", err)
	}
}

// A key that merely starts with the field name is not the field.
func TestFindPasswordFieldDoesNotMatchASimilarKey(t *testing.T) {
	lines := []string{
		"[web]",
		`passwordHashAlgorithm = "pbkdf2"`,
		"# passwordHash is the hashed password, see -pw",
	}

	if _, err := findPasswordField(lines); !errors.Is(err, errFieldMissing) {
		t.Errorf("err = %v, want a similar key and a prose mention to be ignored", err)
	}
}

func TestTableName(t *testing.T) {
	tests := map[string]string{
		"[web]":          "web",
		"  [settings] ":  "settings",
		"[web] # note":   "",
		"listen = \"x\"": "",
		"# [web]":        "",
		"":               "",
	}

	for line, want := range tests {
		if got := tableName(line); got != want {
			t.Errorf("tableName(%q) = %q, want %q", line, got, want)
		}
	}
}

// ── the whole command ───────────────────────────────────────────────────────

// spFixture runs -sp against a config.toml with the given contents, feeding a
// password on stdin the way a script would.
func spFixture(t *testing.T, config string, password string) (dir string, code int) {
	t.Helper()

	dir = t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	saved, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(saved) })

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		write.WriteString(password + "\n")
		write.Close()
	}()

	savedStdin := os.Stdin
	os.Stdin = read
	defer func() { os.Stdin = savedStdin }()

	return dir, writePasswordHash()
}

const spSampleConfig = `# a config with comments that must survive
[server]
type = "debian"
source = ["https://deb.debian.org/debian bookworm main"]

[destination]
arch = ["amd64"]
path = "/tmp/repo"
packages = ["nano"]

[web]
listen = "127.0.0.1:8080"
config = true
user = "admin"
# the hash comes from 'tinyrepo -pw'
passwordHash = ""

[settings]
debug = false
`

func TestSetPasswordWritesTheHash(t *testing.T) {
	dir, code := spFixture(t, spSampleConfig, "a long enough password")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	config, err := loadConfigFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}

	ok, err := verifyPassword(config.Web.PasswordHash, "a long enough password")
	if err != nil || !ok {
		t.Errorf("the written hash does not verify against the password: %v", err)
	}
	if strings.Contains(config.Web.PasswordHash, "a long enough password") {
		t.Error("the plaintext password was written to config.toml")
	}
}

// The reason -sp edits the file textually instead of re-encoding it: the
// comments are the user's, and a command that sets a password must not delete
// them.
func TestSetPasswordKeepsEveryOtherLine(t *testing.T) {
	dir, code := spFixture(t, spSampleConfig, "a long enough password")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}

	written, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}

	before := strings.Split(spSampleConfig, "\n")
	after := strings.Split(string(written), "\n")

	if len(before) != len(after) {
		t.Fatalf("the file went from %d lines to %d", len(before), len(after))
	}
	for i := range before {
		if strings.HasPrefix(strings.TrimSpace(before[i]), passwordField) {
			continue // the one line that is supposed to change
		}
		if before[i] != after[i] {
			t.Errorf("line %d changed:\n before %q\n after  %q", i+1, before[i], after[i])
		}
	}
}

// The requirement: a commented field is an error, and it says which line.
func TestSetPasswordFailsOnACommentedField(t *testing.T) {
	config := `[web]
listen = "127.0.0.1:8080"
# config = true
# user = "admin"
# passwordHash = "pbkdf2-sha256$600000$...$..."
`

	dir, code := spFixture(t, config, "a long enough password")
	if code != 17 {
		t.Errorf("exit code = %d, want 17", code)
	}

	// And nothing was written.
	after, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != config {
		t.Error("a refused -sp still rewrote config.toml")
	}
}

func TestSetPasswordFailsWithoutTheField(t *testing.T) {
	config := "[web]\nlisten = \"127.0.0.1:8080\"\n"

	_, code := spFixture(t, config, "a long enough password")
	if code != 17 {
		t.Errorf("exit code = %d, want 17", code)
	}
}

// A password too short to be worth hashing is refused before anything is
// written, not after.
func TestSetPasswordRefusesAShortPassword(t *testing.T) {
	dir, code := spFixture(t, spSampleConfig, "short")
	if code == 0 {
		t.Error("a five character password was accepted")
	}

	after, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if string(after) != spSampleConfig {
		t.Error("a refused password still rewrote config.toml")
	}
}

// Running it twice has to replace the hash, not append a second field.
func TestSetPasswordReplacesAnExistingHash(t *testing.T) {
	dir, code := spFixture(t, spSampleConfig, "the first password")
	if code != 0 {
		t.Fatalf("first run: exit code = %d", code)
	}
	first, _ := loadConfigFile(filepath.Join(dir, "config.toml"))

	// The fixture chdirs into a fresh directory each time, so the second run
	// works on the file the first one produced.
	written, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}

	dir2, code := spFixture(t, string(written), "the second password")
	if code != 0 {
		t.Fatalf("second run: exit code = %d", code)
	}

	second, err := loadConfigFile(filepath.Join(dir2, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}

	if second.Web.PasswordHash == first.Web.PasswordHash {
		t.Error("the second run did not change the hash")
	}
	if ok, _ := verifyPassword(second.Web.PasswordHash, "the second password"); !ok {
		t.Error("the second password does not verify")
	}
	if ok, _ := verifyPassword(second.Web.PasswordHash, "the first password"); ok {
		t.Error("the first password still works after it was replaced")
	}

	body, _ := os.ReadFile(filepath.Join(dir2, "config.toml"))
	if strings.Count(string(body), "\npasswordHash") != 1 {
		t.Error("the second run added a field instead of replacing the value")
	}
}

// The indentation of the original line is kept, so a nested or aligned config
// does not come back looking rewritten.
func TestSetPasswordKeepsTheIndentation(t *testing.T) {
	config := "[web]\n" +
		"    listen = \"127.0.0.1:8080\"\n" +
		"    user = \"admin\"\n" +
		"    passwordHash = \"\"\n"

	dir, code := spFixture(t, config, "a long enough password")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}

	written, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if !strings.Contains(string(written), "\n    passwordHash = \"pbkdf2") {
		t.Errorf("the indentation was not kept:\n%s", written)
	}
}

func TestSetPasswordReportsAMissingConfig(t *testing.T) {
	dir := t.TempDir()

	saved, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(saved)

	// os.Executable's directory is checked as a fallback, and the test binary
	// lives somewhere without a config.toml, so this reports "not found".
	if code := writePasswordHash(); code != 1 {
		t.Errorf("exit code = %d, want 1 for a missing config.toml", code)
	}
}
