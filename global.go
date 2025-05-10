package main

// DEBUG flags
var DEBUG bool

// CONFIG flags
var slicePackageDeb = make([]PackageDeb, 0)     // packets read from the downloaded cache
var slicePackageDebReal = make([]PackageDeb, 0) // final slice with all packages and their dependencies
var TotalPackagesRead = 0
var SkipDownloadSameSize bool

var packagesIndexSlice = []string{}

var PackagesExtensionPreference = []string{"Packages.xz", "Packages.gz", "Packages"}
var TotalPackagesExtension = len(PackagesExtensionPreference)

const DistsCacheNameUrlBase = "/url_base.txt"
const DestinationDistsName = "tinyrepo"
const DistsCacheName = "/dists_cache"

const DistsName = "/dists"

var ConfigFileToml = "config.toml"
var Language = "en"

var Proxy = ""

// ArCHs
// 0    all
// 1	amd64
// 2    i386

const DefaultConfToml = `[server]
source = [
  "http://deb.debian.org/debian bookworm main contrib",
  "http://security.debian.org/debian-security bookworm-security main"
]

[destination]
# arch = ["amd64", "i386"]
arch = ["amd64"]
# use \\ in windows: path = "C:\\repo\\Downloads"
path = "/tmp/repo/"
packages = ["nano", "wget"]

[proxy]
use = false
host = "http://127.0.0.1"
port = 3128

[settings]
debug = false

# at the moment only check the file size
skipDownloadSameSize = true
`



var DefaultHelp = map[string]string{
	"en": `Tinyrepo general information ;-)
config.toml is searched in the directory where it's executed, and then next to the binary.
You will usually want to use the following order:
tinyrepo -di
tinyrepo -ci
tinyrepo -dp

If you want to see an example of config.toml:
tinyrepo -gc`,
	"es": `Tinyrepo información general ;-)
config.toml es buscado en la ruta donde se ejecuta y después junto al binario
normalmente querrá utilizar el siguiente orden
tinyrepo -di
tinyrepo -ci
tinyrepo -dp

si desea ver un ejemplo de config.toml
tinyrepo -gc`,
}

var messages = map[string]map[string]string{
	"en": {
		"download indexes":  "download indexes",
		"error d":           "error downloading",
		"downloaded":        "downloaded:",
		"starting d":        "starting download:",
		"create indexes":    "create indexes",
		"download packages": "download packages",
		"generate config":   "generate config.toml example",
		"show help":         "show help",
		"err config no f":   "error, 'config.toml' not found",
		"err codes":         "show error codes",
		"config error":      "configuration error",
		"architecture n f":  "architecture not found in:",
		"arch n r":          "architecture not recognizable:",
		"error c P":         "error creating Package:",
		"error w P":         "error writing Package:",
		"err g url_b.txt":   "error generating url_base.txt:",
		"err recursive f":   "recursive search error:",
		"error f exec path": "error getting the executable path",
		"error c p":         "error creating directory:",
		"finished, t e":     "finished, time elapsed:",
		"starting p d":      "starting package download",
		"error c l to d":    "error creating list to download:",
		"creating d d r":    "creating 'dists' to the destination repository",
		"field": "field",
		"invalid p": "invalid proxy",
		"it i m a was n sp": "it is mandatory and was not specified",
		"failed t c HEAD r": "failed to create HEAD request",
		"failed t p HEAD r": "failed to perform HEAD request",
		"unexpected HEAD r": "unexpected status on HEAD request",
		"already d": "already downloaded",
		"error c f": "error creating file",
		"error s c": "error saving content",
		"no p f": "no packages found",
		"error r p": "error reading package",
	},
	"es": {
		"download indexes":  "descargar índices",
		"error d":           "error descargando",
		"downloaded":        "descargado:",
		"starting d":        "iniciando descarga:",
		"create indexes":    "crear índices",
		"download packages": "descargar paquetes",
		"generate config":   "generar ejemplo de config.toml",
		"show help":         "mostrar la ayuda",
		"err config no f":   "error, 'config.toml' no encontrado",
		"err codes":         "mostrar códigos de error",
		"config error":      "error de configuración",
		"architecture n f":  "arquitectura no encontrada en:",
		"arch n r":          "arquitectura no reconocible:",
		"error c P":         "error al crear Package:",
		"error w P":         "error escribiendo Package:",
		"err g url_b.txt":   "error generando url_base.txt:",
		"err recursive f":   "error de búsqueda recursiva:",
		"error f exec path": "error al obtener la ruta del ejecutable:",
		"error c p":         "error creando directorio:",
		"finished, t e":     "terminado, tiempo transcurrido:",
		"starting p d":      "iniciando descarga de paquetes",
		"error c l to d":    "error creando lista ha descargar:",
		"creating d d r":    "creando 'dists' al repositorio de destino",
		"field": "campo",
		"invalid p": "proxy invalido",
		"it i m a was n sp": "es obligatorio y no fue especificado",
		"failed t c HEAD r": "no se pudo crear la solicitud HEAD",
		"failed t p HEAD r": "no se pudo realizar la solicitud HEAD",
		"unexpected HEAD r": "estado inesperado en la solicitud HEAD",
		"already d": "ya descargado",
		"error c f": "error al crear el archivo",
		"error s c": "error al guardar contenido",
		"no p f": "no se encontraron paquetes",
		"error r p": "error leyendo package",
	},
}

var errorCodes = map[string]map[int]string{
	"en": {
		1:  " error, 'config.toml' not found",
		2:  " configuration error in 'config.toml'",
		3:  " architecture not found in (path)",
		4:  " architecture not recognizable (arch)",
		5:  " error creating Package",
		6:  " error writing Package",
		7:  " error generating url_base.txt",
		8:  " recursive search error",
		9:  " error getting the executable path",
		10: "error creating directory",
	},
	"es": {
		1:  " error, 'config.toml' no encontrado",
		2:  " error de configuración en 'config.toml'",
		3:  " arquitectura no encontrada en (ruta)",
		4:  " arquitectura no reconocible (arq)",
		5:  " error al crear Package",
		6:  " error al escribir Package",
		7:  " error generando url_base.txt",
		8:  " error de búsqueda recursiva",
		9:  " error al obtener la ruta del ejecutable",
		10: "error creando directorio",
	},
}
