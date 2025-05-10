package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
)

func main() {

	startTime := time.Now()
	log.SetFlags(log.Ldate | log.Ltime)
	setSystemLanguage()

	var showHelp bool

	flag_di := flag.Bool("di", false, _t("download indexes"))
	flag_ci := flag.Bool("ci", false, _t("create indexes"))
	flag_dp := flag.Bool("dp", false, _t("download packages"))
	flag_gc := flag.Bool("gc", false, _t("generate config"))
	flag_ec := flag.Bool("ec", false, _t("err codes"))

	flag.BoolVar(&showHelp, "help", false, _t("show help"))
	// flag.BoolVar(&showHelp, "h", false, "Alias de -help")

	flag.Parse()

	// show default config
	if *flag_gc {
		fmt.Print(DefaultConfToml)
		os.Exit(0)
	}

	if *flag_ec {
		showErrorCodes()
		os.Exit(0)
	}

	if showHelp {
		showDefaultHelp()
		os.Exit(0)
	}

	ConfigFileToml = getConfigFile()
	if ConfigFileToml == "" {
		fmt.Println(_t("err config no f"))
		os.Exit(1)
	}

	// load default config
	config := defaultConfig()
	if _, err := toml.DecodeFile(ConfigFileToml, &config); err != nil {
		panic(err)
	}

	// validation
	err := validateConfig(&config)
	if err != nil {
		fmt.Println(_t("config error"), ":", err)
		os.Exit(2)
	}

	// overwrite globals
	DEBUG = config.Settings.Debug
	SkipDownloadSameSize = config.Settings.SkipDownloadSameSize
	if config.Proxy.Use {
		Proxy = config.Proxy.Host + ":" + strconv.Itoa(config.Proxy.Port)
	}

	// download indexes
	if *flag_di {
		if DEBUG {
			log.Println(_t("download indexes"))
		}

		downloadIndex(config.Destination.Path, config.Server.Source, config.Destination.Arch)
	}

	// creating dists
	if *flag_ci {
		if DEBUG {
			log.Println(_t("creating d d r"))
		}
		createDists(filepath.Join(config.Destination.Path, DistsCacheName),
			//filepath.Join(config.Destination.Path, DistsName),
			config.Destination.Packages)
		generateDists(config.Destination.Arch, config.Destination.Path)
	}

	// download pool
	if *flag_dp {
		if DEBUG {
			log.Println(_t("starting p d"))
		}
		downloadPool(&config)
	}

	if !*flag_di && !*flag_ci && !*flag_dp && !*flag_gc && !*flag_ec {
		showDefaultHelp()
		os.Exit(0)
	}

	if DEBUG {
		log.Println(_t("finished, t e"), time.Since(startTime))
		// log.Println("Info, total packages on index cache (no the destination repo!)", TotalPackagesRead)
	}

	os.Exit(0)
}

func downloadIndex(destinationPath string, Source []string, Arch []string) {
	destPath := []string{}
	destPatIndex := ""
	destPathUrl := []string{}
	distName := []string{}

	// slices for url_base.txt
	cacheDestPatIndex := []string{}
	cacheDestPathUrl := []string{}

	for _, src := range Source {
		parts := strings.Fields(src) // split string by spaces
		if len(parts) >= 3 {
			for pos, value := range parts {
				if pos > 1 { // los valores después del nombre del lanzamiento
					for _, arch := range Arch {
						//  https://ftp.debian.org/debian/dists/bullseye/main/binary-amd64 /bullseye/main/binary-amd64
						ruta := fmt.Sprintf("%v/dists/%v/%v/binary-%v /%v/%v/binary-%v", parts[0], parts[1], value, arch, parts[1], value, arch)
						packagesIndexSlice = append(packagesIndexSlice, ruta)
						destPathUrl = append(destPathUrl, parts[0])
						distName = append(distName, parts[1])
					}
				}
			}
		}
	}

	// download "Packages" (priority PackagesExtensionPreference "Packages.xz", "Packages.gz", "Packages"")
	for indexPackagesIndexSlice, path := range packagesIndexSlice {
		for _, packageExtension := range PackagesExtensionPreference {
			rut := strings.Fields(path)
			sourceUrl := fmt.Sprintf("%v/%v", rut[0], packageExtension)
			dest := fmt.Sprintf("%v%v/%v", DistsCacheName, rut[1], packageExtension)

			dest = filepath.Join(destinationPath, dest)

			dir := filepath.Dir(dest)
			if !slices.Contains(destPath, dir) {
				destPath = append(destPath, dir)
			}

			if DEBUG {
				log.Println(_t("starting d"), sourceUrl)
			}

			err := downloadFile(sourceUrl, dest)
			if err == nil {
				if DEBUG {
					log.Println(_t("downloaded"), dest)
				}
				destPatIndex = filepath.Join(destinationPath, DistsCacheName)
				destPatIndex = filepath.Join(destPatIndex, distName[indexPackagesIndexSlice])
				destPatIndex = filepath.Join(destPatIndex, DistsCacheNameUrlBase)

				if !slices.Contains(cacheDestPatIndex, destPatIndex) {
					cacheDestPatIndex = append(cacheDestPatIndex, destPatIndex)
					cacheDestPathUrl = append(cacheDestPathUrl, destPathUrl[indexPackagesIndexSlice])
				}
				break
			} else {
				if DEBUG {
					log.Println(_t("error d"), sourceUrl, dest, err)
				}
			}
		}
	}

	// creating url_base.txt
	for index, file := range cacheDestPatIndex {
		// fmt.Println(":>", file, cacheDestPathUrl[index])

		in, err := os.Create(file)
		if err != nil {
			if DEBUG {
				log.Println(_t("err g url_b.txt"), file)
			}
			os.Exit(7)
		}

		in.WriteString(cacheDestPathUrl[index])
		in.Close()
	}

	// decompress
	for _, destPathName := range destPath {
		// check Package first, after: Packages.xz, Packages.gz
		toCheck := filepath.Join(destPathName, PackagesExtensionPreference[TotalPackagesExtension-1])

		if fileExists(toCheck) {
			continue
		} else {
			for _, indexName := range PackagesExtensionPreference[:TotalPackagesExtension-1] {
				toCheck = filepath.Join(destPathName, indexName)
				if fileExists(toCheck) {

					if decompress(toCheck) {
						continue
					} else {
						if DEBUG {
							// TODO: add message decompress fail
						}
					}
				}
			}
		}
	}
}

func appendPackageByName(name string) {
	// TODO: It does not close with a break to find packages for different architectures or updates, it should be improved  later

	for _, namePackage := range slicePackageDeb {
		if namePackage.Package == name {
			for _, namePackageDebReal := range slicePackageDebReal {
				if namePackageDebReal.Package == name {
					return
				}
			}
			slicePackageDebReal = append(slicePackageDebReal, namePackage)
		}
	}
}

func dependencyReader(dependency string) []string {
	// clears all unnecessary data and returns a slice with only the package names
	var dependencyNames []string

	// split be ,
	for t := range strings.SplitSeq(dependency, ",") {

		// separando valores por |
		n := strings.SplitSeq(t, "|")

		for v := range n {
			tmp := strings.TrimLeftFunc(v, unicode.IsSpace)

			// delete right to :
			if i := strings.IndexRune(tmp, ':'); i != -1 {
				tmp = tmp[:i]
			}

			// delete right to (
			if i := strings.IndexRune(tmp, '('); i != -1 {
				tmp = tmp[:i]
			}

			// delete empty spaces
			tmp = strings.ReplaceAll(tmp, " ", "")

			dependencyNames = append(dependencyNames, tmp)
		}
	}
	return dependencyNames
}

func readDebPackages(filePath string) error {
	const maxBufferSize = 1024 * 1024
	buf := make([]byte, maxBufferSize)

	fileNameUrl := ""
	fileUrlDB := filepath.Dir(filepath.Dir(filepath.Dir(filePath)))
	fileUrlDB = filepath.Join(fileUrlDB, DistsCacheNameUrlBase)

	var (
		readPackage, readVersion, readInstalledSize, readMaintainer, readSize, readFilename,
		readMD5sum, readSHA256, readPre_Depends, readDepends, readDescription, readHomepage,
		readTag, readSection, readPriority, readBreaks string
		countTotalPackages int
	)

	// read url_base.txt
	fileDB, err := os.Open(fileUrlDB)
	if err != nil {
		return err
	} else {
		scannerDB := bufio.NewScanner(fileDB)
		scannerDB.Scan()
		fileNameUrl = scannerDB.Text()
	}
	defer fileDB.Close()

	arch := filepath.Base(filepath.Dir(filePath))
	var archCode int8 = 0

	// arch comes from: binary-
	if len(arch) > 7 {
		arch = arch[7:]
	} else {
		if DEBUG {
			log.Println(_t("architecture n f"), filePath)
		}
		os.Exit(3)
	}

	if arch == "amd64" {
		archCode = 1
	}

	if arch == "i386" {
		archCode = 2
	}

	if archCode == 0 {
		if DEBUG {
			log.Println(_t("arch n r"), arch, ":", filePath)
		}
		os.Exit(4)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(buf, maxBufferSize)

	analizarData := func(line string) {
		switch {
		case len(line) > 9 && line[:9] == "Package: ":
			readPackage = line[9:]
		case len(line) > 9 && line[:9] == "Version: ":
			readVersion = line[9:]
		case len(line) > 6 && line[:6] == "Size: ":
			readSize = line[6:]
		case len(line) > 16 && line[:16] == "Installed-Size: ":
			readInstalledSize = line[16:]
		case len(line) > 12 && line[:12] == "Maintainer: ":
			readMaintainer = line[12:]
		case len(line) > 10 && line[:10] == "Filename: ":
			readFilename = line[10:]
		case len(line) > 8 && line[:8] == "MD5sum: ":
			readMD5sum = line[8:]
		case len(line) > 8 && line[:8] == "SHA256: ":
			readSHA256 = line[8:]
		case len(line) > 13 && line[:13] == "Pre-Depends: ":
			readPre_Depends = line[13:]
		case len(line) > 9 && line[:9] == "Depends: ":
			readDepends = line[9:]
		case len(line) > 10 && line[:10] == "Homepage: ":
			readHomepage = line[10:]
		case len(line) > 13 && line[:13] == "Description: ":
			readDescription = line[13:]
		case len(line) > 5 && line[:5] == "Tag: ":
			readTag = line[5:]
		case len(line) > 9 && line[:9] == "Section: ":
			readSection = line[9:]
		case len(line) > 10 && line[:10] == "Priority: ":
			readPriority = line[10:]
		case len(line) > 8 && line[:8] == "Breaks: ":
			readBreaks = line[8:]
		case line == "":
			n := PackageDeb{
				Package:       readPackage,
				Version:       readVersion,
				InstalledSize: readInstalledSize,
				Arch:          archCode,
				Size:          readSize,
				Maintainer:    readMaintainer,
				Filename:      readFilename,
				FilenameUrl:   fileNameUrl,
				MD5sum:        readMD5sum,
				SHA256:        readSHA256,
				Pre_Depends:   readPre_Depends,
				Depends:       readDepends,
				Breaks:        readBreaks,
				Description:   readDescription,
				Homepage:      readHomepage,
				Tag:           readTag,
				Section:       readSection,
				Priority:      readPriority,
				idsDepends:    []int{},           // TODO: use in the future
				id:            TotalPackagesRead, // TODO: use in the future
			}

			TotalPackagesRead++
			countTotalPackages++

			slicePackageDeb = append(slicePackageDeb, n)

			// Reset, to avoid old data in new package.
			readPackage, readVersion, readInstalledSize, readSize, readMaintainer, readFilename,
				readMD5sum, readSHA256, readPre_Depends, readDepends, readDescription, readHomepage,
				readTag, readSection, readPriority, readBreaks = "", "", "", "", "", "", "", "", "",
				"", "", "", "", "", "", ""
		}
	}

	for scanner.Scan() {
		analizarData(scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if countTotalPackages == 0 {
		if DEBUG {
			log.Printf("%s: %v", _t("no p f"), filePath)
		}
	}
	return nil
}

func readDebPackagesToDownload(slicePackageDists []string) ([]string, []string, error) {
	const maxBufferSize = 1024 * 1024
	buf := make([]byte, maxBufferSize)

	sliceDownloadLinks := []string{}
	fileName := []string{}
	readText := ""
	readFilenameUrl := ""

	var errStatus error = nil

	readData := func(line string) {
		switch {
		case len(line) > 10 && line[:10] == "Filename: ":
			readText = line[10:]
		case len(line) > 14 && line[:14] == "#FilenameUrl: ":
			readFilenameUrl = line[14:]
		case line == "":
			fileName = append(fileName, readText)
			sliceDownloadLinks = append(sliceDownloadLinks, readFilenameUrl)
			readText, readFilenameUrl = "", ""
		}
	}


	for _, filePath := range slicePackageDists {

		file, err := os.Open(filePath)
		if err != nil {
			errStatus = err
			continue
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		scanner.Buffer(buf, maxBufferSize)

		

		for scanner.Scan() {
			readData(scanner.Text())
		}

		if err := scanner.Err(); err != nil {
			errStatus = err
			continue
		}
	}

	if errStatus == nil {
		return sliceDownloadLinks, fileName, nil
	} else {
		return nil, nil, errStatus
	}

}

func createDists(packagePath string, packages []string) {
	// packages lista de paquetes a buscar sus dependencias
	var packagesPathSlice []string

	packagesPathSlice, err := recursiveFinder(packagePath, "Packages")

	if err != nil {
		// FIXME: should return error
		return
	}

	for _, packageDistsCache := range packagesPathSlice {
		//fmt.Println("L:>", packageDistsCache)
		err = readDebPackages(packageDistsCache)
		if err != nil {
			if DEBUG {
				log.Printf("%s: %v, error: %v", _t("error r p"), packageDistsCache, err)
			}
		}
	}

	for _, packageRAM := range slicePackageDeb {
		for _, p := range packages {
			if p == packageRAM.Package {
				slicePackageDebReal = append(slicePackageDebReal, packageRAM)
			}
		}
	}

	sizeSliceDestiny := 0
	for i := true; i; {
		sizeSliceDestiny = len(slicePackageDebReal)
		for _, tmpPackage := range slicePackageDebReal {
			for _, dependenceName := range dependencyReader(tmpPackage.Depends) {
				appendPackageByName(dependenceName)
			}
		}
		if sizeSliceDestiny == len(slicePackageDebReal) {
			i = false
		}
	}
	// fmt.Println("sizeSliceDestiny size:", sizeSliceDestiny)
	// fmt.Println(slicePackageDebReal[2])
}

func generateDists(architectures []string, distsPath string) {
	// Create path: dists and Packages

	var dataToWriteBuffer strings.Builder
	dataToWrite := ""

	// creando ruta final donde se guardarán los Packages
	distsPath = filepath.Join(distsPath, "dists")
	distsPath = filepath.Join(distsPath, DestinationDistsName)
	distsPath = filepath.Join(distsPath, "main")

	errPath := os.MkdirAll(distsPath, 0755)
	if errPath != nil {
		if DEBUG {
			fmt.Println(_t("error c p"), errPath)
		}
		os.Exit(10)
	}

	if len(slicePackageDebReal) != 0 {
		var architectureToCheck int8 = 0

		for _, architecturesName := range architectures {
			distsPathDestination := filepath.Join(distsPath, fmt.Sprintf("binary-%v", architecturesName))
			distsPackage := filepath.Join(distsPathDestination, "Packages")
			dataToWrite = ""

			if architecturesName == "amd64" {
				architectureToCheck = 1
			}
			if architecturesName == "i386" {
				architectureToCheck = 2
			}

			errPath := os.MkdirAll(distsPathDestination, 0755)
			if errPath != nil {
				if DEBUG {
					log.Println(_t("error c p"), errPath)
				}
				os.Exit(10)
			}

			// Create file Package
			filePackage, err := os.Create(distsPackage)
			if err != nil {
				if DEBUG {
					log.Println(_t("error c P"), distsPackage)
				}
				os.Exit(5)
			}
			defer filePackage.Close()

			// write content to file
			for _, currentPackage := range slicePackageDebReal {
				if currentPackage.Arch == architectureToCheck {
					// creating buffer
					dataToWriteBuffer.WriteString(
						"Package: " + currentPackage.Package + "\n" +
							"Version: " + currentPackage.Version + "\n" +
							"Installed-Size: " + currentPackage.InstalledSize + "\n" +
							"Maintainer: " + currentPackage.Maintainer + "\n" +
							"Architecture: " + architecturesName + "\n" + // no utilizo el valor del slice para no convertir el  int8
							"Depends: " + currentPackage.Depends + "\n" +
							"Description: " + currentPackage.Description + "\n")
					if currentPackage.Breaks != "" {
						dataToWriteBuffer.WriteString("Breaks: " + currentPackage.Breaks + "\n")
					}
					if currentPackage.Homepage != "" {
						dataToWriteBuffer.WriteString("Homepage: " + currentPackage.Homepage + "\n")
					}
					if currentPackage.Tag != "" {
						dataToWriteBuffer.WriteString("Tag: " + currentPackage.Tag + "\n")
					}
					dataToWriteBuffer.WriteString(
						"Section: " + currentPackage.Section + "\n" +
							"Priority: " + currentPackage.Priority + "\n" +
							"Filename: " + currentPackage.Filename + "\n" +
							"#FilenameUrl: " + currentPackage.FilenameUrl + "\n" +
							"Size: " + currentPackage.Size + "\n" +
							"MD5sum: " + currentPackage.MD5sum + "\n" +
							"SHA256: " + currentPackage.SHA256 + "\n")
					dataToWriteBuffer.WriteString("\n")
				}
			}

			dataToWrite = dataToWriteBuffer.String()
			_, err = filePackage.WriteString(dataToWrite)
			if err != nil {
				if DEBUG {
					log.Println(_t("error w P"), err)
				}
				os.Exit(6)
			}
		}
	}
}

// no f idea, yet
func generateInRelease(architectures []string, distsPath string) {
	// > /dists/tinyrepo/InRelease
	// in progress, no ready

	distsPath = filepath.Join(distsPath, "dists")
	distsPath = filepath.Join(distsPath, DestinationDistsName)
	distsPath = filepath.Join(distsPath, "InRelease")

	//fmt.Println("architectures", architectures)
	//fmt.Println("distsPath", distsPath)

	in, err := os.Create(distsPath)
	if err != nil {
		// error code to define in future
		os.Exit(0)
	}

	defer in.Close()
	in.WriteString("Origin: Tinyrepo \nLabel: Tinyrepo")

}

func downloadPool(config *Config) {
	// download pool

	distsPath := filepath.Join(config.Destination.Path, "dists")
	distsPath = filepath.Join(distsPath, DestinationDistsName)
	distsPath = filepath.Join(distsPath, "main")
	sourceUrl := ""

	slicePackageDists, err := recursiveFinder(distsPath, "Packages")

	if err != nil {
		if DEBUG {
			log.Println(_t("err recursive f"), err)
		}
		os.Exit(8)
	}

	fileName, listaDescarga, err := readDebPackagesToDownload(slicePackageDists)
	if err != nil {
		if DEBUG {
			log.Println(_t("error c l to d"), err)
		}
	}

	for index, dest := range listaDescarga {
		// FIX: replace  + for JOIN
		sourceUrl = fileName[index] + "/" + dest

		// dest = config.Destination.Path + dest
		dest = filepath.Join(config.Destination.Path, dest)
		//fmt.Println("fromDownload =>",  sourceUrl, dest )

		// "" debe ser la configuración del proxy
		err := downloadFile(sourceUrl, dest)
		if err == nil {
			if DEBUG {
				log.Println(_t("downloaded"), dest)
			}
		} else {
			if DEBUG {
				fmt.Println(_t("error d"), err)
			}
		}
	}
}

func getConfigFile() string {
	// return the path of the config.toml, "" if not found.

	if fileExists(ConfigFileToml) {
		return ConfigFileToml
	}

	exePath, err := os.Executable()
	if err != nil {
		if DEBUG {
			log.Println(_t("error f exec path"), err)
		}
		os.Exit(9)
	}
	exeDir := filepath.Dir(exePath)
	ConfigFileToml = filepath.Join(exeDir, ConfigFileToml)

	if fileExists(ConfigFileToml) {
		return ConfigFileToml
	}
	return ""
}
