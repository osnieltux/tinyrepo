package tinyrepo

// DEBUG enables verbose tracing. Errors are always reported regardless.
var DEBUG bool

// SkipDownloadSameSize skips a download when the remote Content-Length matches
// the local file size. Only used for files with no known checksum.
var SkipDownloadSameSize bool

// KeepRawStanzas makes the Debian parser keep each stanza exactly as it was
// read, so it can be republished unchanged. Set from [settings].onDemand.
var KeepRawStanzas bool

var PackagesExtensionPreference = []string{"Packages.xz", "Packages.gz", "Packages"}

const (
	// DistsCacheName is the directory, under the destination path, where the
	// upstream indexes are cached.
	DistsCacheName = "dists_cache"
	// DistsCacheNameUrlBase records which mirror a cached dist came from.
	DistsCacheNameUrlBase = "url_base.txt"
	// ArchCacheName is the directory, under the destination path, where the
	// upstream pacman databases are cached.
	ArchCacheName = "db_cache"

	// DestinationDistsName is the name of the generated repository: the
	// codename for Debian, the repository name for pacman.
	DestinationDistsName = "tinyrepo"
	// DestinationComponent is the only component tinyrepo generates.
	DestinationComponent = "main"

	// ManifestDirName holds tinyrepo's own state inside the destination repo.
	// It is not part of the published repository layout.
	ManifestDirName  = ".tinyrepo"
	ManifestFileName = "manifest.tsv"
	ManifestHeader   = "# tinyrepo manifest v1\n# baseURL\tpoolPath\tsize\tsha256\n"

	// TempSuffix marks a file that is still being written. It has to be kept
	// out of the published repository: on demand, downloading and serving
	// happen at the same time, and a client that picked up a half-written
	// package would be getting a corrupt one.
	TempSuffix = ".tinyrepo.tmp"

	DefaultMaxConcurrentDownloads = 4
	MaxConcurrentDownloadsLimit   = 32

	// MaxIndexSize is the ceiling for anything downloaded or decompressed whose
	// size nothing has told us in advance: repository databases, an upstream
	// Release, and the expansion of a Packages.xz.
	//
	// It is a backstop, not a setting. Generous next to the largest thing that
	// legitimately goes through it - Arch's extra.files, about 50 MB - and
	// still far below filling a disk with what a hostile mirror sends.
	MaxIndexSize = 512 << 20 // 512 MiB

	// DefaultWebListen is where -ws binds. Loopback, so that generating a
	// repository never exposes it to the network by accident.
	DefaultWebListen = "127.0.0.1:8080"

	// MaxConnections is how many connections -ws keeps open at once.
	//
	// A backstop rather than a setting: comfortably above what a package
	// repository ever sees, and comfortably below the usual 1024 file
	// descriptor limit, so a client opening slow connections cannot starve the
	// process of descriptors and stop it answering anybody.
	MaxConnections = 256
)

var ConfigFileToml = "config.toml"
var Language = "en"

// Two ready-to-use examples instead of one file with the other format
// commented out: whichever you print, you can save it and run -di straight
// away.

const DefaultConfTomlDebian = `# tinyrepo - example configuration for a DEBIAN repository
# save it as config.toml, edit it, then run: tinyrepo -di -ci -dp

[server]
type = "debian"

# <mirror> <suite> <component...>
# https, not http: tinyrepo checks each index against the mirror's Release, but
# nothing here verifies a signature, so an attacker able to rewrite the traffic
# could rewrite Release too. The transport is what stops that.
source = [
  "https://deb.debian.org/debian bookworm main contrib",
  "https://security.debian.org/debian-security bookworm-security main"
]

[destination]
# arch = ["amd64", "i386"]
arch = ["amd64"]
# use \\ in windows: path = "C:\\repo\\Downloads"
path = "/tmp/repo/"
# the packages you want; their dependencies are pulled in automatically
packages = ["nano", "wget"]

[web]
# address the -ws web server binds to.
# use "0.0.0.0:8080" to let other machines reach the repository
listen = "127.0.0.1:8080"

# there is a reverse proxy in front (nginx, caddy...) that terminates TLS.
# only then is X-Forwarded-Proto trusted, so that the address printed on the
# repository's home page says https. see the README for the nginx side of it.
# this is NOT the [proxy] section below, which is the proxy used to reach the mirror
behindProxy = false

# publish /healthz: a JSON report with uptime, how many requests were hits and
# misses, how the on-demand downloads are going, and how many connections are
# open. useful for a monitor or a container healthcheck.
# it says nothing a visitor of the repository could not already see, but if you
# would rather keep it private, restrict /healthz in the reverse proxy
health = false

# publish /admin, a web panel that edits this file and runs -di/-ci/-dp/-cl.
# it requires a login: generate the hash with 'tinyrepo -pw'.
# only worth exposing beyond loopback behind TLS - the password is sent as typed
# config = true
# user = "admin"
# passwordHash = "pbkdf2-sha256$600000$...$..."

[proxy]
use = false
host = "http://127.0.0.1"
port = 3128

[settings]
debug = false

# verify the SHA256 published in the index after downloading each package
verifyChecksum = true

# when no checksum is known, skip the download if the remote size matches
skipDownloadSameSize = true

# parallel downloads (1-32)
maxConcurrentDownloads = 4

# publish the whole mirror catalog and let -ws download a package the moment a
# client asks for one that is missing, together with its dependencies.
# packages fetched this way are NOT added to [destination].packages above;
# run 'tinyrepo -cl' to remove them again.
# note: it makes the published index much bigger (Packages goes from a few KB
# to about 52 MB, which is what every 'apt update' then has to read)
onDemand = false
`

const DefaultConfTomlArch = `# tinyrepo - example configuration for an ARCH LINUX / MANJARO repository
# save it as config.toml, edit it, then run: tinyrepo -di -ci -dp

[server]
type = "arch"

# <mirror template> <repository...>
# $repo and $arch are the same placeholders used by /etc/pacman.d/mirrorlist,
# so a Server line from there can be pasted as is.
#
# Manjaro: "https://mirror.alpix.eu/manjaro/stable/$repo/$arch core extra multilib"
source = [
  "https://geo.mirror.pkgbuild.com/$repo/os/$arch core extra multilib"
]

[destination]
arch = ["x86_64"]
# use \\ in windows: path = "C:\\repo\\Downloads"
path = "/tmp/archrepo/"
# package names, or a group name such as "plasma"
packages = ["nano", "wget"]

[web]
# address the -ws web server binds to.
# use "0.0.0.0:8080" to let other machines reach the repository
listen = "127.0.0.1:8080"

# there is a reverse proxy in front (nginx, caddy...) that terminates TLS.
# only then is X-Forwarded-Proto trusted, so that the address printed on the
# repository's home page says https. see the README for the nginx side of it.
# this is NOT the [proxy] section below, which is the proxy used to reach the mirror
behindProxy = false

# publish /healthz: a JSON report with uptime, how many requests were hits and
# misses, how the on-demand downloads are going, and how many connections are
# open. useful for a monitor or a container healthcheck.
# it says nothing a visitor of the repository could not already see, but if you
# would rather keep it private, restrict /healthz in the reverse proxy
health = false

# publish /admin, a web panel that edits this file and runs -di/-ci/-dp/-cl.
# it requires a login: generate the hash with 'tinyrepo -pw'.
# only worth exposing beyond loopback behind TLS - the password is sent as typed
# config = true
# user = "admin"
# passwordHash = "pbkdf2-sha256$600000$...$..."

[proxy]
use = false
host = "http://127.0.0.1"
port = 3128

[settings]
debug = false

# verify the SHA256 published in the database after downloading each package
verifyChecksum = true

# when no checksum is known, skip the download if the remote size matches
skipDownloadSameSize = true

# parallel downloads (1-32)
maxConcurrentDownloads = 4

# also build tinyrepo.files so 'pacman -F' works.
# it makes the metadata download roughly ten times bigger
# (extra.db 8.7 MB -> extra.files 50 MB)
filesDatabase = true

# publish the whole mirror catalog and let -ws download a package the moment a
# client asks for one that is missing, together with its dependencies.
# packages fetched this way are NOT added to [destination].packages above;
# run 'tinyrepo -cl' to remove them again.
# note: it makes the published tinyrepo.db as big as the upstream databases
# it was built from (core + extra is about 8.9 MB)
onDemand = false
`

// DefaultHelp is written out by hand rather than by flag.PrintDefaults, which
// lists the flags alphabetically and puts each description on its own indented
// line. Here they are grouped and ordered the way they are actually used.
var DefaultHelp = map[string]string{
	"en": `tinyrepo - build a small local package repository for Debian or Arch/Manjaro

USAGE
  tinyrepo <options>

  Options can be combined: "tinyrepo -di -ci -dp" does the whole build.
  config.toml is read from the current directory, then from next to the binary.

BUILD THE REPOSITORY  (in this order)
  -di    download the package indexes from the mirror
  -ci    resolve the requested packages and build the repository
  -dp    download the packages themselves
  -ws    serve the repository over HTTP, until Ctrl+C

MAINTENANCE
  -cl    remove packages that config.toml does not ask for
  -vp    check that every package in config.toml exists on the mirror

  -vp reads the index -di cached, so it costs nothing and answers the question
  a typo raises: is this name really there? It exits 18 if any name is not.

WEB CONFIG PANEL
  With config = true in the [web] section, -ws also publishes /admin: a form
  that edits config.toml and buttons that run -di, -ci, -dp and -cl. It needs
  a login, so set [web].user and generate the hash first:

    tinyrepo -pw                 # asks for a password, prints the hash

ON DEMAND
  With onDemand = true in config.toml, -ci publishes the whole mirror catalog
  and -ws downloads a package the first time a client asks for one that is
  missing, pulling in its dependencies in the background. Those packages are
  not added to config.toml, so -cl is what removes them again.

HELP
  -gc    print an example config.toml for Debian
  -ga    print an example config.toml for Arch / Manjaro
  -ec    list the exit codes
  -pw    hash a password for the web config panel, printed to stdout
  -sp    ask for a password and write it into config.toml
  -help  show this help

RUNNING IT AS A SERVICE
  -gs    print a systemd unit         (Debian, Arch, most distributions)
  -gr    print an OpenRC service      (Alpine)

  Both are filled in from where this binary and its config.toml actually are,
  so there is nothing left to edit:

    tinyrepo -gs | sudo tee /etc/systemd/system/tinyrepo.service
    tinyrepo -gr > /etc/init.d/tinyrepo && chmod +x /etc/init.d/tinyrepo

GETTING STARTED
  tinyrepo -gc > config.toml     # or -ga for Arch / Manjaro
  nano config.toml               # set the mirror, the path and the packages
  tinyrepo -di -ci -dp           # build it
  tinyrepo -ws                   # publish it

USING THE REPOSITORY
  It is not signed, so it has to be trusted explicitly.

  Debian, in /etc/apt/sources.list:
    deb [trusted=yes] http://<host>/ tinyrepo main

  Arch / Manjaro, in /etc/pacman.conf:
    [tinyrepo]
    SigLevel = Optional TrustAll
    Server = http://<host>/`,

	"es": `tinyrepo - crea un repositorio local de paquetes para Debian o Arch/Manjaro

USO
  tinyrepo <opciones>

  Las opciones se pueden combinar: "tinyrepo -di -ci -dp" hace todo el proceso.
  config.toml se busca en el directorio actual y después junto al binario.

CREAR EL REPOSITORIO  (en este orden)
  -di    descarga los índices de paquetes del mirror
  -ci    resuelve los paquetes pedidos y crea el repositorio
  -dp    descarga los paquetes
  -ws    sirve el repositorio por HTTP, hasta Ctrl+C

MANTENIMIENTO
  -cl    borra los paquetes que config.toml no pide
  -vp    comprueba que cada paquete de config.toml existe en el mirror

  -vp lee el índice que cacheó -di, así que no cuesta nada y responde a lo que
  provoca una errata: ¿existe de verdad ese nombre? Sale con 18 si alguno no.

BAJO DEMANDA
  Con onDemand = true en config.toml, -ci publica el catálogo completo del
  mirror y -ws descarga el paquete la primera vez que un cliente pide uno que
  falta, trayéndose sus dependencias en segundo plano. Esos paquetes no se
  añaden a config.toml, así que -cl es lo que los vuelve a quitar.

AYUDA
  -gc    muestra un config.toml de ejemplo para Debian
  -ga    muestra un config.toml de ejemplo para Arch / Manjaro
  -ec    lista los códigos de salida
  -pw    genera el hash de una contraseña para el panel web y lo imprime
  -sp    pide una contraseña y la escribe en config.toml
  -help  muestra esta ayuda

EJECUTARLO COMO SERVICIO
  -gs    imprime una unidad de systemd   (Debian, Arch, la mayoría)
  -gr    imprime un servicio de OpenRC   (Alpine)

  Ambos se rellenan con las rutas reales de este binario y de su config.toml,
  así que no queda nada que editar:

    tinyrepo -gs | sudo tee /etc/systemd/system/tinyrepo.service
    tinyrepo -gr > /etc/init.d/tinyrepo && chmod +x /etc/init.d/tinyrepo

PARA EMPEZAR
  tinyrepo -gc > config.toml     # o -ga para Arch / Manjaro
  nano config.toml               # pon el mirror, la ruta y los paquetes
  tinyrepo -di -ci -dp           # créalo
  tinyrepo -ws                   # publícalo

USAR EL REPOSITORIO
  No está firmado, así que hay que confiar en él explícitamente.

  Debian, en /etc/apt/sources.list:
    deb [trusted=yes] http://<host>/ tinyrepo main

  Arch / Manjaro, en /etc/pacman.conf:
    [tinyrepo]
    SigLevel = Optional TrustAll
    Server = http://<host>/`,
}

var messages = map[string]map[string]string{
	"en": {
		"download indexes":       "download indexes",
		"error d":                "error downloading",
		"downloaded":             "downloaded:",
		"starting d":             "starting download:",
		"create indexes":         "create indexes",
		"download packages":      "download packages",
		"generate config":        "print an example config.toml for Debian",
		"generate config arch":   "print an example config.toml for Arch / Manjaro",
		"show help":              "show help",
		"err config no f":        "error, 'config.toml' not found",
		"err codes":              "show error codes",
		"config error":           "configuration error",
		"architecture n f":       "architecture not found in:",
		"error c P":              "error creating Package:",
		"error w P":              "error writing Package:",
		"err g url_b.txt":        "error generating url_base.txt:",
		"err recursive f":        "recursive search error:",
		"error f exec path":      "error getting the executable path",
		"error c p":              "error creating directory:",
		"finished, t e":          "finished, time elapsed:",
		"starting p d":           "starting package download",
		"creating d d r":         "creating 'dists' to the destination repository",
		"field":                  "field",
		"invalid p":              "invalid proxy",
		"invalid a":              "invalid architecture",
		"out of range":           "value out of range",
		"it i m a was n sp":      "it is mandatory and was not specified",
		"failed t c HEAD r":      "failed to create HEAD request",
		"failed t p HEAD r":      "failed to perform HEAD request",
		"unexpected HEAD r":      "unexpected status on HEAD request",
		"unexpected status":      "unexpected HTTP status",
		"already d":              "already downloaded",
		"error c f":              "error creating file",
		"error s c":              "error saving content",
		"no p f":                 "no packages found in",
		"error r p":              "error reading package",
		"err d indexes":          "error downloading indexes",
		"err r indexes":          "error reading the index cache",
		"no p selected":          "no packages were selected, check [destination].packages",
		"unresolved":             "unresolved dependencies:",
		"selected":               "selected packages:",
		"err w manifest":         "error writing the manifest",
		"err r manifest":         "error reading the manifest, run -ci first:",
		"err g release":          "error generating Release",
		"err decompress":         "error decompressing",
		"unknown ext":            "unknown compression extension",
		"checksum mismatch":      "checksum mismatch",
		"size mismatch":          "size mismatch",
		"arch no p":              "no packages for architecture",
		"d failed":               "downloads that failed:",
		"n downloads":            "packages to download:",
		"dup dist":               "two sources share a dist name with different URLs:",
		"unknown type":           "unknown repository type",
		"err r db":               "error reading the package database",
		"no files db":            "no .files database was cached, 'pacman -F' will not work",
		"web server":             "serve the repository over HTTP",
		"invalid listen":         "invalid listen address",
		"serving":                "serving",
		"stopping":               "stopping the web server",
		"err serving":            "error writing the response",
		"err no repo":            "the destination repository does not exist, run -ci first",
		"err web":                "web server error",
		"clean packages":         "remove packages that config.toml does not ask for",
		"err catalog":            "error reading the catalog, run -di first",
		"publishing all":         "on demand, publishing the whole catalog:",
		"on demand ready":        "on demand, packages available:",
		"fetching":               "fetching on demand:",
		"err fetching":           "error fetching on demand",
		"queue full":             "prefetch queue full, skipping:",
		"removed":                "removed:",
		"freed":                  "freed:",
		"nothing to remove":      "nothing to remove",
		"keeping":                "declared packages, kept:",
		"err keep set":           "nothing would be kept, refusing to delete anything",
		"err cleanup":            "cleanup error",
		"err unsafe path":        "refusing a path that leaves the repository:",
		"limit":                  "limit",
		"err release":            "could not read the upstream Release of",
		"err release empty":      "the upstream Release publishes no SHA256 block",
		"release read":           "Release read, files listed:",
		"index unverified":       "the index could not be verified, no checksum published for it",
		"index verified":         "index verified against Release:",
		"busy":                   "too many downloads at once, try again",
		"trying next":            "unusable, trying the next compression variant:",
		"hash password":          "hash a password for the web config panel",
		"set password":           "write the panel password straight into config.toml",
		"verify packages":        "check that every requested package exists on the mirror",
		"pkg found":              "in the index",
		"pkg provided":           "not a package, provided by",
		"pkg group":              "group, members:",
		"pkg missing":            "nothing on the mirror answers to this name",
		"packages ok":            "every requested package exists on the mirror",
		"packages missing":       "requested packages the mirror does not have",
		"err no packages listed": "[destination].packages is empty, there is nothing to check",
		"verify packages now":    "check packages",
		"verify hint":            "checks [destination].packages against the cached index; run -di first, or the indexes action above",
		"package":                "package",
		"downloaded packages":    "downloaded packages",
		"packages page":          "packages on disk",
		"files on disk":          "files on disk",
		"match":                  "match the filter",
		"filter by name":         "filter by name",
		"only undeclared":        "only packages not in the list",
		"filter":                 "filter",
		"version":                "version",
		"size":                   "size",
		"modified":               "modified",
		"declared":               "in the list",
		"adopt":                  "add to list",
		"add to list":            "add this name to [destination].packages",
		"remove from list":       "remove this name from [destination].packages",
		"no packages on disk":    "no package files in the repository yet",
		"previous":               "previous",
		"next":                   "next",
		"page":                   "page",
		"rows":                   "rows",
		"nav config":             "configuration",
		"nav available":          "available",
		"nav downloaded":         "downloaded",
		"nav log":                "log",
		"available packages":     "packages on the mirror",
		"packages on the mirror": "packages on the mirror",
		"in your list":           "in your list",
		"search the mirror":      "search by name or description",
		"only in my list":        "only the ones in my list",
		"description":            "description",
		"nothing matches":        "nothing matches that search",
		"available hint":         "everything the mirror offers, read from the index -di cached. adding a name here only puts it in [destination].packages; run -ci and -dp to actually fetch it.",
		"declare hint":           "the list is what -ci resolves and what -cl keeps. adding a name downloads nothing on its own, and removing one deletes no file: run the actions for that.",
		"err field commented":    "the [web].passwordHash field is commented out on",
		"err field repeated":     "[web].passwordHash appears more than once, on",
		"err field missing":      "there is no passwordHash field under [web] to write to",
		"hint uncomment":         "uncomment it, then run -sp again. it can hold anything, -sp replaces the value",
		"hint add field":         "add passwordHash = \"\" under [web] in config.toml, then run -sp again",
		"password written":       "password written on line",
		"warn no user":           "note: [web].user is empty, the panel has no account to sign in as",
		"warn panel off":         "note: [web].config is false, the panel will not be published",
		"line":                   "line",
		"lines":                  "lines",
		"generate systemd":       "print a systemd unit for this installation",
		"generate openrc":        "print an OpenRC service (Alpine) for this installation",
		"err auth missing":       "the config panel is on but no user/passwordHash is set",
		"err auth hash":          "unusable password hash",
		"err login failed":       "invalid user or password",
		"err login throttled":    "too many failed logins, try again later",
		"err session expired":    "the session expired, sign in again",
		"err job busy":           "another action is already running",
		"err password short":     "the password is too short, minimum characters",
		"err password mismatch":  "the passwords do not match",
		"err reading password":   "could not read the password",
		"prompt password":        "new password",
		"prompt password again":  "repeat the password",
		"password hint":          "copy this into config.toml as [web].passwordHash, and set [web].user and [web].config = true",
		"login ok":               "config panel: signed in from",
		"config saved":           "config panel: configuration saved to",
		"panel at":               "config panel at",
		"panel off":              "config panel off: set config = true under [web] in config.toml (tinyrepo -pw generates the password hash)",
		"panel login":            "sign in",
		"panel config":           "configuration",
		"warn panel exposed":     "WARNING: the config panel is published on a non-loopback address:",
		"warn plain http":        "WARNING: this connection is not encrypted, the password travels in the clear",
		"warn restart":           "changes to [web] and to onDemand apply when the server is restarted; the actions below always use what is saved here",
		"job started":            "started",
		"job failed":             "failed",
		"job done":               "done",
		"unknown action":         "unknown action",
		"actions":                "actions",
		"configuration":          "configuration",
		"action log":             "action log",
		"view log":               "view log",
		"back to config":         "back to configuration",
		"last run":               "last run",
		"no runs yet":            "no action has been run yet",
		"auto refresh":           "this page refreshes itself while the action runs",
		"save config":            "save configuration",
		"sign in":                "sign in",
		"sign out":               "sign out",
		"user":                   "user",
		"password":               "password",
		"new password":           "new password",
		"leave blank to keep":    "leave blank to keep the current one",
		"one per line":           "one per line",
		"help behindProxy":       "a reverse proxy in front terminates TLS, so X-Forwarded-Proto is trusted",
		"help health":            "publish /healthz, a JSON report of what the server is doing",
		"help panel":             "publish this configuration panel",
		"help proxy":             "outbound proxy used to reach the mirror",
		"help verify":            "verify the SHA256 published in the index after each download",
		"help skipsize":          "when no checksum is known, skip the download if the remote size matches",
		"help filesdb":           "generate <repo>.files so \"pacman -F\" works; ignored for Debian",
		"help ondemand":          "publish the whole mirror catalog and download a package when a client asks for it",
		"help debug":             "log every step",
	},
	"es": {
		"download indexes":       "descargar índices",
		"error d":                "error descargando",
		"downloaded":             "descargado:",
		"starting d":             "iniciando descarga:",
		"create indexes":         "crear índices",
		"download packages":      "descargar paquetes",
		"generate config":        "muestra un config.toml de ejemplo para Debian",
		"generate config arch":   "muestra un config.toml de ejemplo para Arch / Manjaro",
		"show help":              "mostrar la ayuda",
		"err config no f":        "error, 'config.toml' no encontrado",
		"err codes":              "mostrar códigos de error",
		"config error":           "error de configuración",
		"architecture n f":       "arquitectura no encontrada en:",
		"error c P":              "error al crear Package:",
		"error w P":              "error escribiendo Package:",
		"err g url_b.txt":        "error generando url_base.txt:",
		"err recursive f":        "error de búsqueda recursiva:",
		"error f exec path":      "error al obtener la ruta del ejecutable:",
		"error c p":              "error creando directorio:",
		"finished, t e":          "terminado, tiempo transcurrido:",
		"starting p d":           "iniciando descarga de paquetes",
		"creating d d r":         "creando 'dists' al repositorio de destino",
		"field":                  "campo",
		"invalid p":              "proxy invalido",
		"invalid a":              "arquitectura inválida",
		"out of range":           "valor fuera de rango",
		"it i m a was n sp":      "es obligatorio y no fue especificado",
		"failed t c HEAD r":      "no se pudo crear la solicitud HEAD",
		"failed t p HEAD r":      "no se pudo realizar la solicitud HEAD",
		"unexpected HEAD r":      "estado inesperado en la solicitud HEAD",
		"unexpected status":      "estado HTTP inesperado",
		"already d":              "ya descargado",
		"error c f":              "error al crear el archivo",
		"error s c":              "error al guardar contenido",
		"no p f":                 "no se encontraron paquetes en",
		"error r p":              "error leyendo package",
		"err d indexes":          "error descargando los índices",
		"err r indexes":          "error leyendo la caché de índices",
		"no p selected":          "no se seleccionó ningún paquete, revise [destination].packages",
		"unresolved":             "dependencias no resueltas:",
		"selected":               "paquetes seleccionados:",
		"err w manifest":         "error escribiendo el manifiesto",
		"err r manifest":         "error leyendo el manifiesto, ejecute -ci primero:",
		"err g release":          "error generando Release",
		"err decompress":         "error descomprimiendo",
		"unknown ext":            "extensión de compresión desconocida",
		"checksum mismatch":      "el checksum no coincide",
		"size mismatch":          "el tamaño no coincide",
		"arch no p":              "sin paquetes para la arquitectura",
		"d failed":               "descargas fallidas:",
		"n downloads":            "paquetes a descargar:",
		"dup dist":               "dos fuentes comparten nombre de dist con URLs distintas:",
		"unknown type":           "tipo de repositorio desconocido",
		"err r db":               "error leyendo la base de datos de paquetes",
		"no files db":            "no se cacheó ninguna base .files, 'pacman -F' no funcionará",
		"web server":             "servir el repositorio por HTTP",
		"invalid listen":         "dirección de escucha inválida",
		"serving":                "sirviendo",
		"stopping":               "deteniendo el servidor web",
		"err serving":            "error al escribir la respuesta",
		"err no repo":            "el repositorio de destino no existe, ejecute -ci primero",
		"err web":                "error del servidor web",
		"clean packages":         "borra los paquetes que config.toml no pide",
		"err catalog":            "error leyendo el catálogo, ejecute -di primero",
		"publishing all":         "bajo demanda, publicando el catálogo completo:",
		"on demand ready":        "bajo demanda, paquetes disponibles:",
		"fetching":               "descargando bajo demanda:",
		"err fetching":           "error descargando bajo demanda",
		"queue full":             "cola de precarga llena, se omite:",
		"removed":                "eliminado:",
		"freed":                  "liberado:",
		"nothing to remove":      "nada que eliminar",
		"keeping":                "paquetes declarados, se conservan:",
		"err keep set":           "no se conservaría nada, no se eliminará nada",
		"err cleanup":            "error durante la limpieza",
		"err unsafe path":        "se rechaza una ruta que sale del repositorio:",
		"limit":                  "límite",
		"err release":            "no se pudo leer el Release del mirror de",
		"err release empty":      "el Release del mirror no publica ningún bloque SHA256",
		"release read":           "Release leído, ficheros listados:",
		"index unverified":       "no se pudo verificar el índice, el mirror no publica su checksum",
		"index verified":         "índice verificado contra Release:",
		"busy":                   "demasiadas descargas a la vez, inténtelo de nuevo",
		"trying next":            "inservible, se prueba la siguiente variante de compresión:",
		"hash password":          "genera el hash de una contraseña para el panel web",
		"set password":           "escribe la contraseña del panel directamente en config.toml",
		"verify packages":        "comprueba que cada paquete pedido existe en el mirror",
		"pkg found":              "en el índice",
		"pkg provided":           "no es un paquete, lo proporciona",
		"pkg group":              "grupo, miembros:",
		"pkg missing":            "no hay nada en el mirror con ese nombre",
		"packages ok":            "todos los paquetes pedidos existen en el mirror",
		"packages missing":       "paquetes pedidos que el mirror no tiene",
		"err no packages listed": "[destination].packages está vacío, no hay nada que comprobar",
		"verify packages now":    "comprobar paquetes",
		"verify hint":            "comprueba [destination].packages contra el índice cacheado; ejecute -di antes, o la acción de índices de arriba",
		"package":                "paquete",
		"downloaded packages":    "paquetes descargados",
		"packages page":          "paquetes en disco",
		"files on disk":          "ficheros en disco",
		"match":                  "coinciden con el filtro",
		"filter by name":         "filtrar por nombre",
		"only undeclared":        "sólo los que no están en la lista",
		"filter":                 "filtrar",
		"version":                "versión",
		"size":                   "tamaño",
		"modified":               "modificado",
		"declared":               "en la lista",
		"adopt":                  "añadir",
		"add to list":            "añade este nombre a [destination].packages",
		"remove from list":       "quita este nombre de [destination].packages",
		"no packages on disk":    "todavía no hay ficheros de paquete en el repositorio",
		"previous":               "anterior",
		"next":                   "siguiente",
		"page":                   "página",
		"rows":                   "filas",
		"nav config":             "configuración",
		"nav available":          "disponibles",
		"nav downloaded":         "descargados",
		"nav log":                "registro",
		"available packages":     "paquetes del mirror",
		"packages on the mirror": "paquetes en el mirror",
		"in your list":           "en tu lista",
		"search the mirror":      "buscar por nombre o descripción",
		"only in my list":        "sólo los de mi lista",
		"description":            "descripción",
		"nothing matches":        "no hay nada que coincida con esa búsqueda",
		"available hint":         "todo lo que ofrece el mirror, leído del índice que cacheó -di. añadir un nombre aquí sólo lo pone en [destination].packages; ejecute -ci y -dp para descargarlo de verdad.",
		"declare hint":           "la lista es lo que resuelve -ci y lo que conserva -cl. añadir un nombre no descarga nada por sí solo, y quitarlo no borra ningún fichero: para eso están las acciones.",
		"err field commented":    "el campo [web].passwordHash está comentado en la",
		"err field repeated":     "[web].passwordHash aparece más de una vez, en las",
		"err field missing":      "no hay ningún campo passwordHash en [web] donde escribir",
		"hint uncomment":         "descoméntelo y vuelva a ejecutar -sp. puede contener cualquier cosa, -sp reemplaza el valor",
		"hint add field":         "añada passwordHash = \"\" en [web] de config.toml y vuelva a ejecutar -sp",
		"password written":       "contraseña escrita en la línea",
		"warn no user":           "aviso: [web].user está vacío, el panel no tiene cuenta con la que entrar",
		"warn panel off":         "aviso: [web].config es false, el panel no se publicará",
		"line":                   "línea",
		"lines":                  "líneas",
		"generate systemd":       "imprime una unidad de systemd para esta instalación",
		"generate openrc":        "imprime un servicio de OpenRC (Alpine) para esta instalación",
		"err auth missing":       "el panel de configuración está activo pero no hay user/passwordHash",
		"err auth hash":          "hash de contraseña inservible",
		"err login failed":       "usuario o contraseña incorrectos",
		"err login throttled":    "demasiados intentos fallidos, inténtelo más tarde",
		"err session expired":    "la sesión ha caducado, vuelva a entrar",
		"err job busy":           "ya hay otra acción en curso",
		"err password short":     "la contraseña es demasiado corta, mínimo de caracteres",
		"err password mismatch":  "las contraseñas no coinciden",
		"err reading password":   "no se pudo leer la contraseña",
		"prompt password":        "nueva contraseña",
		"prompt password again":  "repita la contraseña",
		"password hint":          "copie esto en config.toml como [web].passwordHash, y defina [web].user y [web].config = true",
		"login ok":               "panel de configuración: sesión iniciada desde",
		"config saved":           "panel de configuración: configuración guardada en",
		"panel at":               "panel de configuración en",
		"panel off":              "panel de configuración desactivado: ponga config = true en [web] de config.toml (tinyrepo -pw genera el hash de la contraseña)",
		"panel login":            "iniciar sesión",
		"panel config":           "configuración",
		"warn panel exposed":     "AVISO: el panel de configuración se publica en una dirección que no es loopback:",
		"warn plain http":        "AVISO: esta conexión no está cifrada, la contraseña viaja en claro",
		"warn restart":           "los cambios en [web] y en onDemand se aplican al reiniciar el servidor; las acciones de arriba usan siempre lo guardado aquí",
		"job started":            "iniciado",
		"job failed":             "ha fallado",
		"job done":               "terminado",
		"unknown action":         "acción desconocida",
		"actions":                "acciones",
		"configuration":          "configuración",
		"action log":             "registro de la acción",
		"view log":               "ver registro",
		"back to config":         "volver a la configuración",
		"last run":               "última ejecución",
		"no runs yet":            "todavía no se ha ejecutado ninguna acción",
		"auto refresh":           "esta página se actualiza sola mientras la acción se ejecuta",
		"save config":            "guardar configuración",
		"sign in":                "iniciar sesión",
		"sign out":               "cerrar sesión",
		"user":                   "usuario",
		"password":               "contraseña",
		"new password":           "nueva contraseña",
		"leave blank to keep":    "déjelo vacío para mantener la actual",
		"one per line":           "uno por línea",
		"help behindProxy":       "un proxy inverso delante termina el TLS, así que se confía en X-Forwarded-Proto",
		"help health":            "publica /healthz, un informe JSON de lo que hace el servidor",
		"help panel":             "publica este panel de configuración",
		"help proxy":             "proxy de salida usado para llegar al mirror",
		"help verify":            "verifica el SHA256 publicado en el índice tras cada descarga",
		"help skipsize":          "si no hay checksum, omite la descarga cuando el tamaño remoto coincide",
		"help filesdb":           "genera <repo>.files para que funcione \"pacman -F\"; se ignora en Debian",
		"help ondemand":          "publica el catálogo completo del mirror y descarga un paquete cuando un cliente lo pide",
		"help debug":             "registra cada paso",
	},
}

// Exit codes. Keep in sync with the values returned by run().
var errorCodes = map[string]map[int]string{
	"en": {
		1:  "'config.toml' not found",
		2:  "configuration error in 'config.toml'",
		3:  "error reading the index cache (run -di first)",
		4:  "no packages were selected",
		5:  "error creating a repository index",
		6:  "error writing a repository index",
		7:  "error generating url_base.txt",
		8:  "recursive search error",
		9:  "error getting the executable path",
		10: "error creating a directory",
		11: "error downloading the indexes",
		12: "error downloading the packages",
		13: "error generating Release",
		14: "error writing the manifest",
		15: "web server error",
		16: "cleanup error",
		17: "could not write the password into 'config.toml'",
		18: "some requested packages do not exist on the mirror",
	},
	"es": {
		1:  "'config.toml' no encontrado",
		2:  "error de configuración en 'config.toml'",
		3:  "error leyendo la caché de índices (ejecute -di primero)",
		4:  "no se seleccionó ningún paquete",
		5:  "error al crear un índice del repositorio",
		6:  "error al escribir un índice del repositorio",
		7:  "error generando url_base.txt",
		8:  "error de búsqueda recursiva",
		9:  "error al obtener la ruta del ejecutable",
		10: "error creando un directorio",
		11: "error descargando los índices",
		12: "error descargando los paquetes",
		13: "error generando Release",
		14: "error escribiendo el manifiesto",
		15: "error del servidor web",
		16: "error durante la limpieza",
		17: "no se pudo escribir la contraseña en 'config.toml'",
		18: "algunos paquetes pedidos no existen en el mirror",
	},
}
