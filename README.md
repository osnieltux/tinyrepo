# tinyrepo

A simple command-line application written in **[Go](https://golang.org/)** to create small local repositories for Debian or its derivatives. This application is not intended to be perfect, it may contain bugs. The main goal of this project was to learn Golang.

---

### License
**[GPL v3](https://www.gnu.org/licenses/gpl-3.0.html)**

---

### 🚀 How it works

- #### `./tinyrepo -gc > config.toml` creates a config file
- #### `./tinyrepo -di` downloads indexes from the internet
- #### `./tinyrepo -ci` creates local repository indexes
- #### `./tinyrepo -dp` downloads all packages based on local indexes

---

### 📦 Dependencies
- [Go (version 1.24 or higher)](https://golang.org/dl/)
- [github.com/BurntSushi/toml](https://github.com/BurntSushi/toml) read config
- [github.com/ulikunitz/xz](https://github.com/ulikunitz/xz) decompress .xz files

---

### 🤖 Compilation (Linux, macOS, etc.)
- `go build -ldflags="-s -w" .`

---

### 📝 TODO
- HTTP server  
- `_manifest` for downloads and ensuring local file matches the server  
- checksum verification  
- support multiple repositories  
- improve cache (mini db), optimize index creation  
- separate cache paths from destination repo  
- optimize function to:  
  - remove `#FilenameUrl` from destination repo  
  - compress `Packages` in `.gz` and `.xz`  
  - generate `InRelease`
