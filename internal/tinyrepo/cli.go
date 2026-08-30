package tinyrepo

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// cliFlags holds every command line option.
type cliFlags struct {
	DownloadIndexes *bool
	CreateIndexes   *bool
	DownloadPool    *bool
	WebServer       *bool
	Clean           *bool
	ConfigDebian    *bool
	ConfigArch      *bool
	ErrorCodes      *bool
	Systemd         *bool
	OpenRC          *bool
	Password        *bool
	SetPassword     *bool
	VerifyPackages  *bool
	Help            *bool
}

// anyAction reports whether anything was actually asked for, as opposed to a
// bare "tinyrepo" that should just print the help.
func (f *cliFlags) anyAction() bool {
	return *f.DownloadIndexes || *f.CreateIndexes || *f.DownloadPool ||
		*f.WebServer || *f.Clean
}

// registerFlags declares the options on a flag set. It is a function of its own
// so a test can check that the help documents every one of them.
func registerFlags(set *flag.FlagSet) *cliFlags {
	return &cliFlags{
		DownloadIndexes: set.Bool("di", false, _t("download indexes")),
		CreateIndexes:   set.Bool("ci", false, _t("create indexes")),
		DownloadPool:    set.Bool("dp", false, _t("download packages")),
		WebServer:       set.Bool("ws", false, _t("web server")),
		Clean:           set.Bool("cl", false, _t("clean packages")),
		ConfigDebian:    set.Bool("gc", false, _t("generate config")),
		ConfigArch:      set.Bool("ga", false, _t("generate config arch")),
		ErrorCodes:      set.Bool("ec", false, _t("err codes")),
		Systemd:         set.Bool("gs", false, _t("generate systemd")),
		OpenRC:          set.Bool("gr", false, _t("generate openrc")),
		Password:        set.Bool("pw", false, _t("hash password")),
		SetPassword:     set.Bool("sp", false, _t("set password")),
		VerifyPackages:  set.Bool("vp", false, _t("verify packages")),
		Help:            set.Bool("help", false, _t("show help")),
	}
}

// Main runs the command line interface and returns the process exit code.
func Main() int {
	startTime := time.Now()
	log.SetFlags(log.Ldate | log.Ltime)
	setSystemLanguage()

	options := registerFlags(flag.CommandLine)

	// Makes -h, -help and an unknown flag all print the same thing.
	flag.Usage = showDefaultHelp
	flag.Parse()

	if *options.ConfigDebian {
		fmt.Print(DefaultConfTomlDebian)
		return 0
	}

	if *options.ConfigArch {
		fmt.Print(DefaultConfTomlArch)
		return 0
	}

	if *options.Systemd {
		showSystemdUnit()
		return 0
	}

	if *options.OpenRC {
		showOpenRCScript()
		return 0
	}

	if *options.ErrorCodes {
		showErrorCodes()
		return 0
	}

	if *options.Password {
		return promptPasswordHash()
	}

	if *options.SetPassword {
		return writePasswordHash()
	}

	if *options.VerifyPackages {
		return showPackageCheck()
	}

	if *options.Help || !options.anyAction() {
		showDefaultHelp()
		return 0
	}

	configPath, err := getConfigFile()
	if err != nil {
		logErrorf("%s: %v", _t("error f exec path"), err)
		return 9
	}
	if configPath == "" {
		logError(_t("err config no f"))
		return 1
	}
	ConfigFileToml = configPath

	config := defaultConfig()
	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		logErrorf("%s: %v", _t("config error"), err)
		return 2
	}

	if err := validateConfig(&config); err != nil {
		logErrorf("%s: %v", _t("config error"), err)
		return 2
	}

	DEBUG = config.Settings.Debug
	SkipDownloadSameSize = config.Settings.SkipDownloadSameSize
	// Only on demand, where the published index is the whole archive: apt then
	// acts on fields the resolver never reads, so the stanzas have to be
	// republished exactly as upstream wrote them. It doubles what a full suite
	// costs in memory, which is why it is not on by default.
	KeepRawStanzas = config.Settings.OnDemand

	if err := initHTTPClient(&config); err != nil {
		logErrorf("%s: %v", _t("config error"), err)
		return 2
	}

	backend, err := newBackend(config.Server.Type)
	if err != nil {
		logErrorf("%s: %v", _t("config error"), err)
		return 2
	}

	if *options.DownloadIndexes {
		logDebug(_t("download indexes"))
		if err := backend.FetchIndexes(&config); err != nil {
			logErrorf("%s: %v", _t("err d indexes"), err)
			return 11
		}
	}

	if *options.CreateIndexes {
		logDebug(_t("creating d d r"))

		if err := backend.Build(&config); err != nil {
			if errors.Is(err, errNoPackagesSelected) {
				logError(_t("no p selected"))
			} else {
				logErrorf("%s: %v", _t("config error"), err)
			}

			// The backend picks the documented exit code for the stage that
			// failed, so the CLI does not need to know the format.
			var failure *buildError
			if errors.As(err, &failure) {
				return failure.Code
			}
			return 6
		}
	}

	if *options.DownloadPool {
		logDebug(_t("starting p d"))
		if err := downloadPool(&config); err != nil {
			logErrorf("%s: %v", _t("error d"), err)
			return 12
		}
	}

	// After -dp, so that "-dp -cl" downloads what is declared and then drops
	// whatever else had accumulated.
	if *options.Clean {
		if err := cleanRepo(&config, backend); err != nil {
			logErrorf("%s: %v", _t("err cleanup"), err)
			return 16
		}
	}

	// Last, so that "-ci -ws" builds the repository and then publishes it.
	// This blocks until interrupted.
	if *options.WebServer {
		if err := serveRepo(&config, backend, configPath); err != nil {
			logErrorf("%s: %v", _t("err web"), err)
			return 15
		}
	}

	logDebug(_t("finished, t e"), time.Since(startTime))
	return 0
}

// getConfigFile looks for config.toml in the working directory first, then
// next to the binary. Returns "" when neither exists.
func getConfigFile() (string, error) {
	if fileExists(ConfigFileToml) {
		return ConfigFileToml, nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}

	beside := filepath.Join(filepath.Dir(exePath), ConfigFileToml)
	if fileExists(beside) {
		return beside, nil
	}
	return "", nil
}
