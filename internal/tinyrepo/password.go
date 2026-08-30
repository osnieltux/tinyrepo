package tinyrepo

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// minPasswordLen is the shortest password the panel accepts. PBKDF2 makes
// guessing expensive; it does not make "1234" safe.
const minPasswordLen = 8

// promptPasswordHash implements -pw: it reads a password and prints the line to
// paste into config.toml. The password itself is never written anywhere - not
// to the config, not to the log, not to the shell history, which is why this is
// a prompt rather than an argument.
func promptPasswordHash() int {
	password, code := askPassword()
	if code != 0 {
		return code
	}

	hash, err := hashPassword(password)
	if err != nil {
		logErrorf("%s: %v", _t("err auth hash"), err)
		return 2
	}

	// To stdout, alone, so it can be piped straight into a config file.
	fmt.Println(hash)

	if isTerminal() {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, _t("password hint"))
	}
	return 0
}

// readSecret reads one line, with terminal echo off when there is a terminal to
// turn it off on.
func readSecret(stdin *bufio.Reader, piped bool) (string, error) {
	if !piped {
		// stty rather than a dependency for the one call that needs it. If it
		// is not there the password echoes, which is worse than a failure but
		// better than not being able to set one at all.
		if err := setEcho(false); err == nil {
			defer func() {
				setEcho(true)
				fmt.Fprintln(os.Stderr)
			}()
		}
	}

	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func setEcho(on bool) error {
	arg := "-echo"
	if on {
		arg = "echo"
	}

	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// isTerminal reports whether stdin is a terminal rather than a pipe or a file.
func isTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ── writing the hash into config.toml ───────────────────────────────────────

// passwordField is the key -sp writes. It only counts inside [web]: the same
// name under another table would be a different setting.
const passwordField = "passwordHash"

// fieldSite is where the field was found while scanning config.toml.
type fieldSite struct {
	// Line is 1-indexed, so it matches what an editor shows.
	Line int
	// Indent is whatever whitespace preceded the key, kept so the rewritten
	// line sits exactly where the original did.
	Indent string
	// Commented is true for a line that is only an example, such as
	// "# passwordHash = ...". Those are refused rather than uncommented:
	// writing a live credential into a line the user has switched off would be
	// changing a decision they made, and silently.
	Commented bool
	// InWeb says the field was found under [web] rather than another table.
	InWeb bool
}

var (
	errFieldMissing   = errors.New("no passwordHash field")
	errFieldCommented = errors.New("the passwordHash field is commented out")
	errFieldRepeated  = errors.New("more than one passwordHash field")
)

// fieldError carries which lines are at fault separately from the sentinel, so
// the message the user reads is assembled from the translation table rather
// than from an English error string with a translated prefix glued to it.
type fieldError struct {
	kind  error
	lines []int
}

func (e *fieldError) Error() string { return e.kind.Error() + ": " + e.Lines() }
func (e *fieldError) Unwrap() error { return e.kind }

// Lines renders the offending line numbers, with the right word for one or
// several.
func (e *fieldError) Lines() string {
	word := _t("line")
	if len(e.lines) > 1 {
		word = _t("lines")
	}

	numbers := make([]string, 0, len(e.lines))
	for _, line := range e.lines {
		numbers = append(numbers, strconv.Itoa(line))
	}
	return word + " " + strings.Join(numbers, ", ")
}

func newFieldError(kind error, sites []fieldSite) *fieldError {
	lines := make([]int, 0, len(sites))
	for _, site := range sites {
		lines = append(lines, site.Line)
	}
	return &fieldError{kind: kind, lines: lines}
}

// tableName returns the table a "[section]" line opens, or "" for any other
// line. Only the bare form is recognised, which is what tinyrepo writes and
// what both sample configs use.
func tableName(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return ""
	}
	return strings.TrimSpace(trimmed[1 : len(trimmed)-1])
}

// findPasswordField locates the passwordHash line in a config file.
//
// It is deliberately textual rather than a TOML decode and re-encode: the file
// is full of comments the user wrote and expects to still be there afterwards,
// and re-encoding would throw every one of them away.
func findPasswordField(lines []string) (fieldSite, error) {
	var live, commented []fieldSite
	table := ""

	for i, line := range lines {
		if name := tableName(line); name != "" {
			table = name
			continue
		}

		trimmed := strings.TrimLeft(line, " \t")
		indent := line[:len(line)-len(trimmed)]

		isComment := strings.HasPrefix(trimmed, "#")
		if isComment {
			// Every leading marker, so "## passwordHash" and "#   passwordHash"
			// are both recognised as the commented-out field they are.
			trimmed = strings.TrimLeft(trimmed, "# \t")
		}

		// "passwordHash" followed by optional space and "=", so that a key
		// merely mentioning it in prose is not mistaken for the setting.
		if !strings.HasPrefix(trimmed, passwordField) {
			continue
		}
		rest := strings.TrimLeft(trimmed[len(passwordField):], " \t")
		if !strings.HasPrefix(rest, "=") {
			continue
		}

		site := fieldSite{Line: i + 1, Indent: indent, Commented: isComment, InWeb: table == "web"}
		if isComment {
			commented = append(commented, site)
		} else {
			live = append(live, site)
		}
	}

	// A live field under [web] is the one case that can be written.
	var inWeb []fieldSite
	for _, site := range live {
		if site.InWeb {
			inWeb = append(inWeb, site)
		}
	}

	switch {
	case len(inWeb) == 1:
		return inWeb[0], nil

	case len(inWeb) > 1:
		// Ambiguous: TOML would take the first and error on the duplicate, and
		// guessing which one the user meant is not this command's job.
		return fieldSite{}, newFieldError(errFieldRepeated, inWeb)

	case len(commented) > 0:
		return fieldSite{}, newFieldError(errFieldCommented, commented)

	case len(live) > 0:
		// Present, but under some other table, so writing it there would set a
		// field nothing reads.
		return fieldSite{}, newFieldError(errFieldMissing, live)
	}

	return fieldSite{}, errFieldMissing
}

// writePasswordHash implements -sp: it asks for a password and writes its hash
// into the existing passwordHash line of config.toml, leaving every other line,
// and every comment, exactly as it was.
func writePasswordHash() int {
	configPath, err := getConfigFile()
	if err != nil {
		logErrorf("%s: %v", _t("error f exec path"), err)
		return 9
	}
	if configPath == "" {
		logError(_t("err config no f"))
		return 1
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		logErrorf("%s: %v", _t("config error"), err)
		return 2
	}

	// Split before asking for anything: refusing after the password has been
	// typed twice wastes the user's time for no reason.
	lines := strings.Split(string(raw), "\n")

	site, err := findPasswordField(lines)
	if err != nil {
		// The line numbers are the point of the message: without them the user
		// is told something is wrong with a file and left to find where.
		where := ""
		var field *fieldError
		if errors.As(err, &field) {
			where = " " + field.Lines()
		}

		switch {
		case errors.Is(err, errFieldCommented):
			logErrorf("%s: %s%s", configPath, _t("err field commented"), where)
			logError(_t("hint uncomment"))
		case errors.Is(err, errFieldRepeated):
			logErrorf("%s: %s%s", configPath, _t("err field repeated"), where)
		default:
			logErrorf("%s: %s%s", configPath, _t("err field missing"), where)
			logError(_t("hint add field"))
		}
		return 17
	}

	password, code := askPassword()
	if code != 0 {
		return code
	}

	hash, err := hashPassword(password)
	if err != nil {
		logErrorf("%s: %v", _t("err auth hash"), err)
		return 2
	}

	lines[site.Line-1] = fmt.Sprintf("%s%s = %s", site.Indent, passwordField, quote(hash))

	// Atomic, like every other write: a config.toml truncated halfway through
	// is one the next run refuses to start with.
	body := strings.Join(lines, "\n")
	if err := writeFileAtomic(configPath, func(w io.Writer) error {
		_, err := io.WriteString(w, body)
		return err
	}); err != nil {
		logErrorf("%s: %v", _t("config error"), err)
		return 17
	}

	logErrorf("%s: %s %d", configPath, _t("password written"), site.Line)
	warnPanelIncomplete(configPath)
	return 0
}

// warnPanelIncomplete says what is still missing for the panel to come up. A
// password on its own does not publish it, and finding that out from a silent
// -ws is worse than being told here.
func warnPanelIncomplete(configPath string) {
	config := defaultConfig()
	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		// The hash went in on the line that was there, so a file that does not
		// parse was already not parsing. Nothing useful to add.
		return
	}

	if strings.TrimSpace(config.Web.User) == "" {
		logError(_t("warn no user"))
	}
	if !config.Web.Config {
		logError(_t("warn panel off"))
	}
}

// askPassword prompts twice and returns the password, or a non-zero exit code.
// Shared by -pw and -sp so both behave identically at the prompt.
func askPassword() (string, int) {
	stdin := bufio.NewReader(os.Stdin)
	piped := !isTerminal()

	if !piped {
		fmt.Fprint(os.Stderr, _t("prompt password")+": ")
	}

	first, err := readSecret(stdin, piped)
	if err != nil {
		logErrorf("%s: %v", _t("err reading password"), err)
		return "", 2
	}

	if len(first) < minPasswordLen {
		logErrorf("%s (%d)", _t("err password short"), minPasswordLen)
		return "", 2
	}

	if piped {
		return first, 0
	}

	fmt.Fprint(os.Stderr, _t("prompt password again")+": ")

	second, err := readSecret(stdin, piped)
	if err != nil {
		logErrorf("%s: %v", _t("err reading password"), err)
		return "", 2
	}
	if first != second {
		logError(_t("err password mismatch"))
		return "", 2
	}
	return first, 0
}
