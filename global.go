package main

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

ON DEMAND
  With onDemand = true in config.toml, -ci publishes the whole mirror catalog
  and -ws downloads a package the first time a client asks for one that is
  missing, pulling in its dependencies in the background. Those packages are
  not added to config.toml, so -cl is what removes them again.

HELP
  -gc    print an example config.toml for Debian
  -ga    print an example config.toml for Arch / Manjaro
  -ec    list the exit codes
  -help  show this help

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

BAJO DEMANDA
  Con onDemand = true en config.toml, -ci publica el catálogo completo del
  mirror y -ws descarga el paquete la primera vez que un cliente pide uno que
  falta, trayéndose sus dependencias en segundo plano. Esos paquetes no se
  añaden a config.toml, así que -cl es lo que los vuelve a quitar.

AYUDA
  -gc    muestra un config.toml de ejemplo para Debian
  -ga    muestra un config.toml de ejemplo para Arch / Manjaro
  -ec    lista los códigos de salida
  -help  muestra esta ayuda

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
		"download indexes":     "download indexes",
		"error d":              "error downloading",
		"downloaded":           "downloaded:",
		"starting d":           "starting download:",
		"create indexes":       "create indexes",
		"download packages":    "download packages",
		"generate config":      "print an example config.toml for Debian",
		"generate config arch": "print an example config.toml for Arch / Manjaro",
		"show help":            "show help",
		"err config no f":      "error, 'config.toml' not found",
		"err codes":            "show error codes",
		"config error":         "configuration error",
		"architecture n f":     "architecture not found in:",
		"error c P":            "error creating Package:",
		"error w P":            "error writing Package:",
		"err g url_b.txt":      "error generating url_base.txt:",
		"err recursive f":      "recursive search error:",
		"error f exec path":    "error getting the executable path",
		"error c p":            "error creating directory:",
		"finished, t e":        "finished, time elapsed:",
		"starting p d":         "starting package download",
		"creating d d r":       "creating 'dists' to the destination repository",
		"field":                "field",
		"invalid p":            "invalid proxy",
		"invalid a":            "invalid architecture",
		"out of range":         "value out of range",
		"it i m a was n sp":    "it is mandatory and was not specified",
		"failed t c HEAD r":    "failed to create HEAD request",
		"failed t p HEAD r":    "failed to perform HEAD request",
		"unexpected HEAD r":    "unexpected status on HEAD request",
		"unexpected status":    "unexpected HTTP status",
		"already d":            "already downloaded",
		"error c f":            "error creating file",
		"error s c":            "error saving content",
		"no p f":               "no packages found in",
		"error r p":            "error reading package",
		"err d indexes":        "error downloading indexes",
		"err r indexes":        "error reading the index cache",
		"no p selected":        "no packages were selected, check [destination].packages",
		"unresolved":           "unresolved dependencies:",
		"selected":             "selected packages:",
		"err w manifest":       "error writing the manifest",
		"err r manifest":       "error reading the manifest, run -ci first:",
		"err g release":        "error generating Release",
		"err decompress":       "error decompressing",
		"unknown ext":          "unknown compression extension",
		"checksum mismatch":    "checksum mismatch",
		"size mismatch":        "size mismatch",
		"arch no p":            "no packages for architecture",
		"d failed":             "downloads that failed:",
		"n downloads":          "packages to download:",
		"dup dist":             "two sources share a dist name with different URLs:",
		"unknown type":         "unknown repository type",
		"err r db":             "error reading the package database",
		"no files db":          "no .files database was cached, 'pacman -F' will not work",
		"web server":           "serve the repository over HTTP",
		"invalid listen":       "invalid listen address",
		"serving":              "serving",
		"stopping":             "stopping the web server",
		"err serving":          "error writing the response",
		"err no repo":          "the destination repository does not exist, run -ci first",
		"err web":              "web server error",
		"clean packages":       "remove packages that config.toml does not ask for",
		"err catalog":          "error reading the catalog, run -di first",
		"publishing all":       "on demand, publishing the whole catalog:",
		"on demand ready":      "on demand, packages available:",
		"fetching":             "fetching on demand:",
		"err fetching":         "error fetching on demand",
		"queue full":           "prefetch queue full, skipping:",
		"removed":              "removed:",
		"freed":                "freed:",
		"nothing to remove":    "nothing to remove",
		"keeping":              "declared packages, kept:",
		"err keep set":         "nothing would be kept, refusing to delete anything",
		"err cleanup":          "cleanup error",
		"err unsafe path":      "refusing a path that leaves the repository:",
		"limit":                "limit",
		"err release":          "could not read the upstream Release of",
		"err release empty":    "the upstream Release publishes no SHA256 block",
		"release read":         "Release read, files listed:",
		"index unverified":     "the index could not be verified, no checksum published for it",
		"index verified":       "index verified against Release:",
		"busy":                 "too many downloads at once, try again",
		"trying next":          "unusable, trying the next compression variant:",
	},
	"es": {
		"download indexes":     "descargar índices",
		"error d":              "error descargando",
		"downloaded":           "descargado:",
		"starting d":           "iniciando descarga:",
		"create indexes":       "crear índices",
		"download packages":    "descargar paquetes",
		"generate config":      "muestra un config.toml de ejemplo para Debian",
		"generate config arch": "muestra un config.toml de ejemplo para Arch / Manjaro",
		"show help":            "mostrar la ayuda",
		"err config no f":      "error, 'config.toml' no encontrado",
		"err codes":            "mostrar códigos de error",
		"config error":         "error de configuración",
		"architecture n f":     "arquitectura no encontrada en:",
		"error c P":            "error al crear Package:",
		"error w P":            "error escribiendo Package:",
		"err g url_b.txt":      "error generando url_base.txt:",
		"err recursive f":      "error de búsqueda recursiva:",
		"error f exec path":    "error al obtener la ruta del ejecutable:",
		"error c p":            "error creando directorio:",
		"finished, t e":        "terminado, tiempo transcurrido:",
		"starting p d":         "iniciando descarga de paquetes",
		"creating d d r":       "creando 'dists' al repositorio de destino",
		"field":                "campo",
		"invalid p":            "proxy invalido",
		"invalid a":            "arquitectura inválida",
		"out of range":         "valor fuera de rango",
		"it i m a was n sp":    "es obligatorio y no fue especificado",
		"failed t c HEAD r":    "no se pudo crear la solicitud HEAD",
		"failed t p HEAD r":    "no se pudo realizar la solicitud HEAD",
		"unexpected HEAD r":    "estado inesperado en la solicitud HEAD",
		"unexpected status":    "estado HTTP inesperado",
		"already d":            "ya descargado",
		"error c f":            "error al crear el archivo",
		"error s c":            "error al guardar contenido",
		"no p f":               "no se encontraron paquetes en",
		"error r p":            "error leyendo package",
		"err d indexes":        "error descargando los índices",
		"err r indexes":        "error leyendo la caché de índices",
		"no p selected":        "no se seleccionó ningún paquete, revise [destination].packages",
		"unresolved":           "dependencias no resueltas:",
		"selected":             "paquetes seleccionados:",
		"err w manifest":       "error escribiendo el manifiesto",
		"err r manifest":       "error leyendo el manifiesto, ejecute -ci primero:",
		"err g release":        "error generando Release",
		"err decompress":       "error descomprimiendo",
		"unknown ext":          "extensión de compresión desconocida",
		"checksum mismatch":    "el checksum no coincide",
		"size mismatch":        "el tamaño no coincide",
		"arch no p":            "sin paquetes para la arquitectura",
		"d failed":             "descargas fallidas:",
		"n downloads":          "paquetes a descargar:",
		"dup dist":             "dos fuentes comparten nombre de dist con URLs distintas:",
		"unknown type":         "tipo de repositorio desconocido",
		"err r db":             "error leyendo la base de datos de paquetes",
		"no files db":          "no se cacheó ninguna base .files, 'pacman -F' no funcionará",
		"web server":           "servir el repositorio por HTTP",
		"invalid listen":       "dirección de escucha inválida",
		"serving":              "sirviendo",
		"stopping":             "deteniendo el servidor web",
		"err serving":          "error al escribir la respuesta",
		"err no repo":          "el repositorio de destino no existe, ejecute -ci primero",
		"err web":              "error del servidor web",
		"clean packages":       "borra los paquetes que config.toml no pide",
		"err catalog":          "error leyendo el catálogo, ejecute -di primero",
		"publishing all":       "bajo demanda, publicando el catálogo completo:",
		"on demand ready":      "bajo demanda, paquetes disponibles:",
		"fetching":             "descargando bajo demanda:",
		"err fetching":         "error descargando bajo demanda",
		"queue full":           "cola de precarga llena, se omite:",
		"removed":              "eliminado:",
		"freed":                "liberado:",
		"nothing to remove":    "nada que eliminar",
		"keeping":              "paquetes declarados, se conservan:",
		"err keep set":         "no se conservaría nada, no se eliminará nada",
		"err cleanup":          "error durante la limpieza",
		"err unsafe path":      "se rechaza una ruta que sale del repositorio:",
		"limit":                "límite",
		"err release":          "no se pudo leer el Release del mirror de",
		"err release empty":    "el Release del mirror no publica ningún bloque SHA256",
		"release read":         "Release leído, ficheros listados:",
		"index unverified":     "no se pudo verificar el índice, el mirror no publica su checksum",
		"index verified":       "índice verificado contra Release:",
		"busy":                 "demasiadas descargas a la vez, inténtelo de nuevo",
		"trying next":          "inservible, se prueba la siguiente variante de compresión:",
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
	},
}
