package tinyrepo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// -gs and -gr print an init file for the two service managers, the same way -gc
// prints a config: to stdout, complete, and with nothing left to fill in.
//
// They are filled from where this binary and its config.toml actually are, and
// from the destination path the config asks for, so what comes out is what this
// installation needs rather than a template with placeholders in it.

const (
	// serviceUser is the unprivileged account both files run as. tinyrepo binds
	// a port above 1024 and writes only its own repository, so it never needs
	// to be root.
	serviceUser = "tinyrepo"
	// serviceName is the unit and the OpenRC service name.
	serviceName = "tinyrepo"
)

// serviceFacts is everything the two templates need about this installation.
type serviceFacts struct {
	// Exec is the absolute path of this binary.
	Exec string
	// WorkDir is the directory holding config.toml, because that is where
	// tinyrepo looks for it.
	WorkDir string
	// Repo is [destination].path: the one directory the service writes to.
	Repo string
	// Listen is [web].listen, only used to warn in a comment when the service
	// would come up bound to loopback.
	Listen string
	// Found says whether a config was actually read, so the file can say which
	// values are real and which are the documented defaults.
	Found bool
}

func gatherServiceFacts() serviceFacts {
	facts := serviceFacts{
		Exec:    "/usr/local/bin/tinyrepo",
		WorkDir: "/etc/tinyrepo",
		Repo:    "/var/lib/tinyrepo",
		Listen:  DefaultWebListen,
	}

	// The running binary is very often already where it will be installed, and
	// when it is not, an absolute path is still a better starting point than a
	// guess.
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		facts.Exec = exe
	}

	configPath, err := getConfigFile()
	if err != nil || configPath == "" {
		return facts
	}

	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return facts
	}
	facts.WorkDir = filepath.Dir(absolute)

	// A config that does not load is not a reason to fail: the point of these
	// files is to be generated before everything is in place.
	config, err := loadConfigFile(absolute)
	if err != nil {
		return facts
	}

	facts.Found = true
	facts.Listen = config.Web.Listen
	if path, err := filepath.Abs(config.Destination.Path); err == nil {
		facts.Repo = path
	}
	return facts
}

// source describes where a value came from, so the generated file says whether
// a path is this installation's or a default to be edited.
func (f serviceFacts) source() string {
	if f.Found {
		return "from config.toml"
	}
	return "no config.toml was found, these are defaults - check them"
}

// loopbackNote warns, in the file itself, about the one thing that reliably
// surprises people: a service that starts fine and answers nobody.
func (f serviceFacts) loopbackNote() string {
	if !isLoopback(f.Listen) {
		return ""
	}
	return fmt.Sprintf(
		"# note: [web].listen is %s, so the repository will only answer on this\n"+
			"# machine. set it to 0.0.0.0:8080 to let other hosts reach it.\n", f.Listen)
}

// showSystemdUnit implements -gs.
func showSystemdUnit() {
	facts := gatherServiceFacts()

	fmt.Printf(`# tinyrepo - systemd unit  (%s)
#
#   sudo useradd --system --home-dir %s --shell /usr/sbin/nologin %s
#   sudo install -d -o %s -g %s %s %s
#   tinyrepo -gs | sudo tee /etc/systemd/system/%s.service
#   sudo systemctl daemon-reload
#   sudo systemctl enable --now %s
#
%s
[Unit]
Description=tinyrepo package repository
Documentation=https://github.com/osniel/tinyrepo
# network-online rather than network: on demand reaches the mirror the moment a
# client asks for a package it does not have.
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
ExecStart=%s -ws
WorkingDirectory=%s
User=%s
Group=%s

# -ws stops cleanly on SIGTERM, finishing whatever it is still streaming.
KillSignal=SIGTERM
TimeoutStopSec=30
Restart=on-failure
RestartSec=5

# The service needs to write exactly one directory. Everything below is what it
# does NOT need, so that a bug in a parser reading a hostile mirror's index has
# as little to reach for as possible.
ReadWritePaths=%s
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
NoNewPrivileges=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
ProtectProc=invisible
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service
CapabilityBoundingSet=
# A port below 1024 needs this line uncommented instead of the empty set above:
# CapabilityBoundingSet=CAP_NET_BIND_SERVICE
# AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
`,
		facts.source(),
		shQuote(facts.Repo), serviceUser,
		serviceUser, serviceUser, shQuote(facts.Repo), shQuote(facts.WorkDir),
		serviceName,
		serviceName,
		facts.loopbackNote(),
		sdQuote(facts.Exec),
		sdQuote(facts.WorkDir),
		serviceUser, serviceUser,
		sdQuote(facts.Repo),
	)
}

// showOpenRCScript implements -gr: the Alpine init script.
func showOpenRCScript() {
	facts := gatherServiceFacts()

	fmt.Printf(`#!/sbin/openrc-run
# tinyrepo - OpenRC service for Alpine  (%s)
#
#   adduser -S -D -H -h %s -s /sbin/nologin %s
#   install -d -o %s -g %s %s %s
#   tinyrepo -gr > /etc/init.d/%s
#   chmod +x /etc/init.d/%s
#   rc-update add %s default
#   rc-service %s start
#
%s
name="tinyrepo"
description="tinyrepo package repository"

command=%s
command_args="-ws"
# -ws runs in the foreground until it is stopped, so OpenRC is the one that
# backgrounds it and owns the pidfile.
command_background=true
command_user="%s:%s"
directory=%s

pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/${RC_SVCNAME}.log"
error_log="/var/log/${RC_SVCNAME}.log"

# -ws finishes what it is streaming before it exits, so give it room to.
retry="SIGTERM/30/SIGKILL/5"

depend() {
	# on demand reaches the mirror while it is serving, not only at startup.
	need net
	after firewall
}

start_pre() {
	checkpath --directory --owner %s:%s --mode 0755 "${directory}"
	checkpath --directory --owner %s:%s --mode 0755 %s
	checkpath --file      --owner %s:%s --mode 0644 "${output_log}"
}
`,
		facts.source(),
		shQuote(facts.Repo), serviceUser,
		serviceUser, serviceUser, shQuote(facts.Repo), shQuote(facts.WorkDir),
		serviceName, serviceName, serviceName, serviceName,
		facts.loopbackNote(),
		shQuote(facts.Exec),
		serviceUser, serviceUser,
		shQuote(facts.WorkDir),
		serviceUser, serviceUser,
		serviceUser, serviceUser, shQuote(facts.Repo),
		serviceUser, serviceUser,
	)
}

// shQuote renders a path as a single-quoted shell word, which is what the
// OpenRC script needs for a destination path containing a space. Single quotes
// take everything literally, so only a single quote itself has to be escaped.
func shQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// sdQuote renders a path for systemd. A unit splits ExecStart on whitespace, so
// a path with a space in it has to arrive quoted, and systemd uses the same
// backslash escaping inside double quotes that C does.
func sdQuote(value string) string {
	if !strings.ContainsAny(value, " \t\"'\\") {
		return value
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
	return `"` + escaped + `"`
}
