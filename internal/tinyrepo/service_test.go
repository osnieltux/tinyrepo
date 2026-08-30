package tinyrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn and returns everything it printed. The generators write
// to stdout by design - the whole point is "tinyrepo -gs | tee" - so that is
// what has to be inspected.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	saved := os.Stdout
	os.Stdout = write

	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := read.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()

	fn()
	write.Close()
	os.Stdout = saved

	return <-done
}

// serviceFixture runs a generator from inside a directory holding a real
// config.toml, which is what getConfigFile looks for.
func serviceFixture(t *testing.T, mutate func(*Config)) string {
	t.Helper()

	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")

	config := defaultConfig()
	config.Server.Source = []string{"https://deb.debian.org/debian bookworm main"}
	config.Destination.Path = repo
	config.Destination.Packages = []string{"nano"}
	if mutate != nil {
		mutate(&config)
	}

	if err := saveConfigFile(filepath.Join(dir, "config.toml"), &config); err != nil {
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

	return dir
}

// The unit has to name this installation's paths, not a placeholder: a file
// that still has to be edited by hand is one more thing to get wrong.
func TestSystemdUnitUsesTheRealPaths(t *testing.T) {
	dir := serviceFixture(t, nil)
	unit := captureStdout(t, showSystemdUnit)

	repo := filepath.Join(dir, "repo")

	for _, want := range []string{
		"ExecStart=",
		" -ws",
		"WorkingDirectory=" + dir,
		"ReadWritePaths=" + repo,
		"User=" + serviceUser,
		"[Install]",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit does not contain %q", want)
		}
	}

	if strings.Contains(unit, "<") || strings.Contains(unit, "CHANGEME") {
		t.Error("the unit still has a placeholder in it")
	}
}

// The service writes exactly one directory, and the hardening is the reason to
// generate the unit rather than let people write ExecStart by hand.
func TestSystemdUnitIsHardened(t *testing.T) {
	serviceFixture(t, nil)
	unit := captureStdout(t, showSystemdUnit)

	for _, want := range []string{
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"ProtectHome=true",
		"PrivateTmp=true",
		"RestrictAddressFamilies=AF_INET AF_INET6",
		"SystemCallFilter=@system-service",
		"CapabilityBoundingSet=",
		"MemoryDenyWriteExecute=true",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit is missing %q", want)
		}
	}

	// Running as root would undo every line above it.
	if strings.Contains(unit, "User=root") {
		t.Error("the unit runs the service as root")
	}
}

// A repository reached on demand talks to the mirror while it serves, so the
// unit has to wait for a usable network rather than only for a configured one.
func TestSystemdUnitWaitsForTheNetwork(t *testing.T) {
	serviceFixture(t, nil)
	unit := captureStdout(t, showSystemdUnit)

	if !strings.Contains(unit, "After=network-online.target") ||
		!strings.Contains(unit, "Wants=network-online.target") {
		t.Error("the unit does not wait for the network to be up")
	}
}

func TestOpenRCScriptUsesTheRealPaths(t *testing.T) {
	dir := serviceFixture(t, nil)
	script := captureStdout(t, showOpenRCScript)

	for _, want := range []string{
		"#!/sbin/openrc-run",
		`command_args="-ws"`,
		"command_background=true",
		`command_user="` + serviceUser + ":" + serviceUser + `"`,
		"directory=",
		dir,
		"depend()",
		"need net",
		"start_pre()",
		"checkpath",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script does not contain %q", want)
		}
	}

	// The shebang has to be the very first thing, or the file is not executable
	// as a script at all.
	if !strings.HasPrefix(script, "#!/sbin/openrc-run\n") {
		t.Error("the script does not start with its shebang")
	}
}

// -ws finishes what it is streaming when it is asked to stop, so neither file
// should cut a download short by killing it immediately.
func TestServiceFilesAllowACleanStop(t *testing.T) {
	serviceFixture(t, nil)

	unit := captureStdout(t, showSystemdUnit)
	if !strings.Contains(unit, "KillSignal=SIGTERM") || !strings.Contains(unit, "TimeoutStopSec=") {
		t.Error("the unit does not give the server time to shut down cleanly")
	}

	script := captureStdout(t, showOpenRCScript)
	if !strings.Contains(script, "retry=") {
		t.Error("the OpenRC script does not give the server time to shut down cleanly")
	}
}

// A path with a space in it is the case that silently produces a unit systemd
// parses as two arguments, and an init script that checkpaths the wrong
// directory.
func TestServiceFilesQuotePathsWithSpaces(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo with spaces")

	serviceFixture(t, func(c *Config) { c.Destination.Path = repo })

	unit := captureStdout(t, showSystemdUnit)
	if !strings.Contains(unit, `ReadWritePaths="`) {
		t.Errorf("the unit does not quote a path containing a space:\n%s", unit)
	}

	script := captureStdout(t, showOpenRCScript)
	if !strings.Contains(script, "'") {
		t.Error("the OpenRC script does not quote a path containing a space")
	}
}

func TestSdQuote(t *testing.T) {
	tests := map[string]string{
		"/usr/local/bin/tinyrepo": "/usr/local/bin/tinyrepo",
		"/tmp/repo with spaces":   `"/tmp/repo with spaces"`,
		`/tmp/back\slash`:         `"/tmp/back\\slash"`,
		`/tmp/quo"te`:             `"/tmp/quo\"te"`,
	}

	for in, want := range tests {
		if got := sdQuote(in); got != want {
			t.Errorf("sdQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShQuote(t *testing.T) {
	tests := map[string]string{
		"/var/lib/tinyrepo":     "'/var/lib/tinyrepo'",
		"/tmp/repo with spaces": "'/tmp/repo with spaces'",
		"/tmp/it's":             `'/tmp/it'\''s'`,
		"/tmp/$HOME":            "'/tmp/$HOME'",
	}

	for in, want := range tests {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// Generating a service file is something people do before everything is in
// place, so neither generator may need a working config to produce output.
func TestServiceFilesAreGeneratedWithoutAConfig(t *testing.T) {
	dir := t.TempDir()

	saved, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(saved)

	unit := captureStdout(t, showSystemdUnit)
	script := captureStdout(t, showOpenRCScript)

	if !strings.Contains(unit, "[Service]") {
		t.Error("no unit was produced without a config.toml")
	}
	if !strings.HasPrefix(script, "#!/sbin/openrc-run") {
		t.Error("no OpenRC script was produced without a config.toml")
	}

	// And each says that the paths in it are defaults rather than this
	// installation's, so they are not pasted in unread.
	for _, out := range []string{unit, script} {
		if !strings.Contains(out, "no config.toml was found") {
			t.Error("the file does not say that its paths are defaults")
		}
	}
}

// A broken config.toml must not stop a service file being generated - that is
// often exactly when one is needed.
func TestServiceFilesSurviveABrokenConfig(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("not toml {{"), 0600); err != nil {
		t.Fatal(err)
	}

	saved, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(saved)

	if unit := captureStdout(t, showSystemdUnit); !strings.Contains(unit, "[Service]") {
		t.Error("a broken config.toml prevented the unit from being generated")
	}
	if script := captureStdout(t, showOpenRCScript); !strings.HasPrefix(script, "#!/sbin/openrc-run") {
		t.Error("a broken config.toml prevented the OpenRC script from being generated")
	}
}

// A service bound to loopback starts perfectly and answers nobody. Saying so in
// the file is the cheapest place to catch it.
func TestServiceFilesWarnAboutLoopback(t *testing.T) {
	serviceFixture(t, nil) // the default listen is 127.0.0.1:8080

	for name, out := range map[string]string{
		"systemd": captureStdout(t, showSystemdUnit),
		"openrc":  captureStdout(t, showOpenRCScript),
	} {
		if !strings.Contains(out, "only answer on this") {
			t.Errorf("%s: no warning about a loopback listen address", name)
		}
	}
}

func TestServiceFilesDoNotWarnWhenPublished(t *testing.T) {
	serviceFixture(t, func(c *Config) { c.Web.Listen = "0.0.0.0:8080" })

	for name, out := range map[string]string{
		"systemd": captureStdout(t, showSystemdUnit),
		"openrc":  captureStdout(t, showOpenRCScript),
	} {
		if strings.Contains(out, "only answer on this") {
			t.Errorf("%s: warns about loopback for a published listen address", name)
		}
	}
}
