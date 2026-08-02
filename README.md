# tinyrepo

A simple command-line application written in **[Go](https://golang.org/)** to create small local repositories for **Debian** (and its derivatives) and for **Arch Linux / Manjaro**. This application is not intended to be perfect, it may contain bugs. The main goal of this project was to learn Golang.

I started tinyrepo on my own, as a way to learn Go, and later improved it with
the help of an AI assistant.

Pick the format with `type` in `config.toml`; the commands are the same for both.

---

### License
**[GPL v3](https://www.gnu.org/licenses/gpl-3.0.html)**

---

### 🚀 How it works

```sh
mv tinyrepo-linux-amd64 tinyrepo && chmod +x tinyrepo

./tinyrepo -gc > config.toml   # example config for Debian
./tinyrepo -ga > config.toml   # ...or for Arch / Manjaro
nano config.toml               # set the mirror, the path and the packages

./tinyrepo -di -ci -dp         # download indexes, build the repo, download packages
./tinyrepo -ws                 # publish it over HTTP
```

`-gc` and `-ga` each print a complete, ready-to-use `config.toml` for that
format - no commented-out alternatives to uncomment.

| | |
| --- | --- |
| `-di` | download the package indexes from the mirror |
| `-ci` | resolve the requested packages and build the repository |
| `-dp` | download the packages themselves |
| `-ws` | serve the repository over HTTP, until `Ctrl+C` |
| `-cl` | remove packages that `config.toml` does not ask for |
| `-gc` / `-ga` | print an example `config.toml` for Debian / Arch |
| `-ec` | list the exit codes |
| `-help` | show the help |

The flags compose, so `./tinyrepo -ci -ws` rebuilds the repository and then
publishes it. `-ws` runs until you stop it with `Ctrl+C`, and shuts down cleanly.

Neither generated repository is signed, so each one has to be trusted explicitly.

**Debian** — add it to `sources.list`:

```text
deb [trusted=yes] http://<host>/ tinyrepo main
```

**Arch / Manjaro** — add it to `/etc/pacman.conf`:

```ini
[tinyrepo]
SigLevel = Optional TrustAll
Server = http://<host>/
```

The individual packages keep their upstream `%PGPSIG%`, so pacman still reports
`Validated By: Signature` for them.

---

### 📡 On-demand mode

By default the repository is closed: it holds the packages named in
`config.toml` and their dependencies, and nothing else. Set `onDemand = true` and
it becomes open — a client can ask for anything the mirror publishes, and
tinyrepo fetches it at that moment.

```toml
[settings]
onDemand = true
```

What changes:

- **`-ci` publishes the whole mirror catalog** instead of just the resolved
  closure. This is the part that makes the mode work at all: apt and pacman only
  ever request what they can see in the index, so they have to see everything.
- **`-dp` still downloads only the declared closure.** The index is complete, the
  disk is not.
- **`-ws` fetches on a miss.** The package is streamed to the client and written
  to disk at the same time, so the client never sits on a silent connection, and
  it is only kept once its `SHA256` matches the one published in the index. Its
  dependencies are fetched in the background so the next request is a hit.

Packages that arrive this way are **not** written back into `config.toml`.
`-cl` is what removes them again.

```sh
./tinyrepo -di -ci -dp     # build: index of everything, disk holds the declared closure
./tinyrepo -ws             # publish; missing packages are fetched as they are asked for
./tinyrepo -cl             # drop everything config.toml does not ask for
```

`-cl` recomputes the declared closure from `config.toml` and the cached index, so
it needs `-di` to have run. It prints every file it removes with its size and a
freed total. It only ever considers real package files — `pool/**` for Debian and
`*.pkg.tar*` for pacman — plus the `.tinyrepo.tmp` leftovers of a download that
was cut off, so `Release`, `Packages`, `tinyrepo.db`, the caches and the manifest
are out of its reach by construction. It refuses to run if the
declared packages resolve to nothing, which means a stale cache or a typo rather
than a request to empty the repository.

**The cost.** A complete index is much bigger than a curated one, and it is what
every `apt update` downloads:

| | published index | on disk |
| --- | --- | --- |
| Debian `bookworm main amd64` | `Packages` 48 MB, `.gz` 12 MB | ~60 MB |
| Arch `core` + `extra` | `tinyrepo.db` ~8.9 MB | ~9 MB |

`Packages.xz` is **not** generated in this mode: the pure Go xz writer needs
about a minute for a 48 MB index, for a variant apt is happy to do without as
long as `Release` does not promise it. With `filesDatabase = true`, `-ci` also
builds the *full* upstream `.files`, which is roughly ten times the `.db`.

**The ceiling.** `maxConcurrentDownloads` is also the limit on how many packages
are being fetched at once across *every* client, not just during `-dp`. Past it a
request waits up to 30 seconds and is then answered `503` with `Retry-After`, so
a burst sheds load instead of opening one upstream connection per package
requested. Background prefetching always yields to a real request. Measured
against the real mirror, sixty simultaneous requests for distinct packages held
at exactly four upstream connections.

**Two caveats.** Concurrent requests for the same missing package produce one
download, and the others wait for it to finish — over a slow uplink a second
client can time out on a large package, though its retry is then a hit. And a
`Range` request on a miss is answered with the whole file rather than a `206`;
from the second request on, the file is local and ranges work normally.

---

### 🌐 Web server

`-ws` publishes the destination directory with plain HTML directory listings -
no CSS, no JavaScript, nothing a text browser cannot render. Opening the root
page also prints the `sources.list` or `pacman.conf` snippet for whichever
repository was generated.

It serves byte ranges and `If-Modified-Since`, which is what lets apt and pacman
resume an interrupted download and revalidate their caches. Only `GET` and
`HEAD` are answered.

The upstream caches (`dists_cache/`, `db_cache/`) and tinyrepo's own state
(`.tinyrepo/`) live inside the destination directory but are **not** published:
they are not part of the repository. Set `debug = true` for an access log.

```toml
[web]
# use "0.0.0.0:8080" to let other machines reach the repository
listen = "127.0.0.1:8080"
```

It binds to loopback by default, so building a repository never exposes it to
the network by accident. This is a plain static file server: put it behind a
real web server if you need TLS, logging or access control.

At most 256 connections are accepted at once. Past that the rest wait in the
kernel's accept queue rather than each costing a goroutine and a file
descriptor — a request that misses on demand can hold its connection for half a
minute while it waits for a download slot, so an unbounded server would be easy
to starve.

#### Behind nginx, with a certificate

Point tinyrepo at loopback and let nginx own the port, the certificate and the
outside world:

```toml
[web]
listen = "127.0.0.1:8080"
# only now is X-Forwarded-Proto believed, so the home page prints https://
behindProxy = true
```

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name repo.example.com;

    ssl_certificate     /etc/letsencrypt/live/repo.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/repo.example.com/privkey.pem;

    # Packages are large, and on demand they are still being fetched while they
    # are being sent. With buffering on, nginx reads the whole package before
    # forwarding any of it: the client sits on a silent connection, which is
    # exactly what pacman gives up on after about ten seconds.
    proxy_buffering off;
    proxy_max_temp_file_size 0;

    # proxy_read_timeout is the gap allowed between reads from tinyrepo, and on
    # a miss nothing is written until the mirror answers: up to 30s queueing for
    # a download slot, or however long another client's download of the same
    # package takes, which has no limit of its own. The 60s default turns that
    # into a 504 on a large package.
    proxy_read_timeout 300s;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;

        # clientHints builds the sources.list line from this.
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

```sh
certbot --nginx -d repo.example.com
```

Three things that are easy to get wrong:

- **Give it its own `server_name`, serving at `/`.** A directory request without
  a trailing slash is answered with a redirect built from the path tinyrepo
  received, so if nginx strips a prefix (`location /repo/ { proxy_pass ...; }`)
  that redirect sends the client somewhere that does not exist. The links inside
  a listing are relative and would survive; the redirect is the one that breaks.
- **Do not add `proxy_cache`.** tinyrepo already caches to disk, so a second
  cache doubles the storage and can keep serving a stale index after `-ci`.
- **`[proxy]` in `config.toml` is not this.** That section is the *outbound*
  proxy used to reach the mirror. There is no setting for the reverse proxy
  beyond `behindProxy`.

`behindProxy` is off by default because `X-Forwarded-Proto` is set by whoever
connects, and if the port is reachable directly that is anybody. Leave it off
unless a proxy really is in front, and keep `listen` on loopback when it is.

#### Health endpoint

A running `-ws` otherwise says nothing about whether it is working: with `debug`
off there is no log, and with `debug` on there is a log to read rather than a
number to watch. `health = true` publishes `/healthz`:

```toml
[web]
health = true
```

```console
$ curl -s http://127.0.0.1:8080/healthz
{
  "status": "ok",
  "uptimeSeconds": 3821,
  "repo": {
    "type": "debian",
    "path": "/tmp/repo/",
    "onDemand": true,
    "catalog": 63436
  },
  "requests": {
    "hits": 1204,
    "misses": 87,
    "notFound": 3
  },
  "downloads": {
    "ok": 84,
    "failed": 2,
    "busy": 1,
    "inFlight": 2,
    "slots": 4
  },
  "connections": {
    "open": 7,
    "max": 256
  }
}
```

`hits` were served from disk and `misses` were fetched on demand, so the ratio
between them is how full the repository is for what its clients actually ask
for. `busy` counts the requests turned away with a 503 because no download slot
came free in time, and `inFlight` against `slots` is how close it is to that
happening again — a rising `busy` is the sign to raise
`maxConcurrentDownloads`. `downloads` is absent altogether when on-demand mode
is off, because zeroes would read as *nothing is being fetched* rather than
*nothing can be*.

The status is `ok`, or `degraded` with a **503** when the destination directory
is no longer there — a repository that was moved or unmounted underneath a
running server still answers requests, and is the failure worth catching. So a
container healthcheck, or a `systemd` watchdog, needs nothing more than the
status code:

```sh
curl -fsS http://127.0.0.1:8080/healthz > /dev/null
```

Requests to `/healthz` are not counted as repository traffic, so a monitor
polling it every few seconds does not inflate its own numbers. The path is
matched before the filesystem, so a file of that name could not shadow it;
neither repository format has one.

It is off by default because it is a second thing the port answers. Nothing in
it is secret — counters about a repository that is already public — but if you
would rather it not be, keep it to the proxy:

```nginx
location = /healthz {
    allow 127.0.0.1;
    deny  all;
    proxy_pass http://127.0.0.1:8080;
}
```

---

### 🔒 What is and is not verified

Packages are verified against the `SHA256` published in the index, and index
downloads are verified against the checksums the mirror publishes in its own
`Release` — both the compressed file and its expansion, which also bounds how
far a compressed index is allowed to decompress. When a mirror publishes no
`Release`, or does not list a file, tinyrepo says so and carries on unverified.

**Nothing here checks a signature.** That means this catches a corrupt mirror or
a mangled file, but not an attacker who can rewrite the traffic, because they
can rewrite `Release` too. Use `https` in `[server].source` — the sample does —
until GPG verification lands. Arch publishes no checksum for its databases at
all, so there the transport is the only protection.

Two consequences of streaming a package to the client while it downloads: the
first client to request a missing package receives bytes before the checksum has
been verified, and a body that then fails verification is not cached. Both apt
and pacman verify checksums themselves against the index, so they reject a bad
body regardless.

Paths coming from a mirror (`Filename:`, `%FILENAME%`) are rejected if they
would write outside the destination directory.

---

### ⚙️ Configuration

`config.toml` is looked up in the working directory first, then next to the binary.

`[server].type` is `"debian"` or `"arch"`, and it decides how `source` is read:

```toml
# type = "debian":  <mirror> <suite> <component...>
source = ["https://deb.debian.org/debian bookworm main contrib"]

# type = "arch":  <mirror template> <repository...>
# $repo and $arch are the same placeholders as /etc/pacman.d/mirrorlist,
# so a Server line can be pasted as is - it covers both layouts.
# one or the other, not both: source is a single key
source = ["https://geo.mirror.pkgbuild.com/$repo/os/$arch core extra multilib"]
# source = ["https://mirror.alpix.eu/manjaro/stable/$repo/$arch core extra multilib"]
```

For `type = "arch"` set `arch = ["x86_64"]`, and `packages` may name either a
package or a group (`plasma`, `kde-applications`).

| `[destination]` | default | meaning |
| --- | --- | --- |
| `path` | — | where the repository is built. The upstream caches and `.tinyrepo/` live inside it |
| `arch` | `["amd64"]` | architectures to publish. `all` and `any` are rejected: they are not indexes of their own, those packages live inside every concrete architecture's index |
| `packages` | — | the packages you want; their dependencies are pulled in automatically |

| `[settings]` | default | meaning |
| --- | --- | --- |
| `debug` | `false` | verbose tracing (errors are always reported) |
| `verifyChecksum` | `true` | check each package against the `SHA256` published in the index |
| `skipDownloadSameSize` | `true` | for files with no known checksum, skip when the remote size matches |
| `maxConcurrentDownloads` | `4` | parallel downloads (1-32) |
| `filesDatabase` | `true` | `arch` only: also build `tinyrepo.files` so `pacman -F` works. Makes the metadata download ~10x bigger |
| `onDemand` | `false` | publish the whole mirror catalog and fetch a package when a client asks for it. See [On-demand mode](#-on-demand-mode) |

`maxConcurrentDownloads` is also the ceiling on how many packages `-ws` fetches
at once across every client, not only during `-dp`.

| `[web]` | default | meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:8080` | address `-ws` binds to; `0.0.0.0:8080` to reach it from other machines |
| `behindProxy` | `false` | a reverse proxy terminates the connection, so `X-Forwarded-Proto` decides the scheme printed on the home page. See [Behind nginx](#behind-nginx-with-a-certificate) |
| `health` | `false` | publish `/healthz`, a JSON report of uptime, hits and misses, downloads and open connections. See [Health endpoint](#health-endpoint) |

| `[proxy]` | default | meaning |
| --- | --- | --- |
| `use` | `false` | send every download through an HTTP proxy |
| `host` | — | the proxy address, **with its scheme**: `"http://127.0.0.1"`. Without one it is rejected |
| `port` | — | 1-65535 |

`use = false` does not mean *no proxy*: the HTTP client is built with Go's
`ProxyFromEnvironment`, so `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` are honoured
either way. What this section does is force *this* proxy instead, ignoring the
environment. It is the **outbound** proxy used to reach the mirror — the reverse
proxy in front of `-ws` is `[web].behindProxy`, further up.

**Languages.** Messages are printed in English or Spanish, chosen from `LC_ALL`,
then `LC_MESSAGES`, then `LANG` (`Get-Culture` on Windows). There is no setting
for it, so run `LC_ALL=en_US.UTF-8 tinyrepo ...` to force English on a translated
system.

**Known limitation.** Version constraints are dropped when resolving
(`glibc>=2.34` is treated as `glibc`). The goal is to have the package present in
the mirror; matching exact versions is the package manager's job at install time.

---

### 📦 Dependencies
- [Go (version 1.24 or higher)](https://golang.org/dl/)
- [github.com/BurntSushi/toml](https://github.com/BurntSushi/toml) read config
- [github.com/ulikunitz/xz](https://github.com/ulikunitz/xz) decompress .xz files

---

### 🤖 Compilation (Linux, macOS, etc.)
- `go build -ldflags="-s -w" .`

---

### 🧪 Tests

```sh
go test -short ./...        # unit tests only, no network
go test ./...               # everything, including the integration tests
go test -run Integration -v ./...
```

The integration tests download real slices of both archives into
`/tmp/tinyrepo-integration` and run the real code paths against them: Debian's
`bookworm/contrib/binary-amd64` index (~53 KB) plus a few ~3 KB `.deb` files, and
Arch's `core` database (~130 KB) plus a small `.pkg.tar.zst`. The downloads are
cached there and reused across runs; delete the directory to start from cold.
Override the location with `TINYREPO_TEST_DIR`.

They skip - never fail - when the mirror is unreachable, so an offline checkout
still runs green.

---

### ✅ Done
- **on-demand mode** (`onDemand`): the index advertises the whole mirror and `-ws` fetches a package the first time it is asked for, streaming it to the client while it downloads  
- **cleanup** (`-cl`): removes everything `config.toml` does not ask for, including packages upstream has since superseded  
- index downloads are **verified against the mirror's own `Release`**, compressed file and expansion both  
- a mirror cannot write outside the destination: `Filename:` and `%FILENAME%` are rejected if they escape it  
- every download and decompression is **bounded**, so a runaway or compressed-bomb index cannot fill the disk  
- on-demand fetches share one global limit (`maxConcurrentDownloads`), with `503` rather than an unbounded queue  
- **built-in HTTP server** (`-ws`), with classic HTML listings, byte ranges and conditional requests  
- **health endpoint** (`health`): `/healthz` reports hits, misses, downloads and open connections, and turns `503` when the repository directory is gone  
- **ready to sit behind nginx** (`behindProxy`): the home page prints the `https://` address the client actually reached, not the plain one nginx spoke to  
- at most **256 connections** at once, so slow connections cannot starve the process of file descriptors  
- **Arch / Manjaro support**: `tinyrepo.db` + `tinyrepo.files`, so `pacman -S` and `pacman -F` both work  
- virtual packages resolve through `Provides:` / `%PROVIDES%`, including pacman sonames (`libreadline.so=8-64`)  
- package **groups** can be used as a seed (`plasma`)  
- `.tinyrepo/manifest.tsv` drives the downloads and records size + `SHA256`  
- checksum verification (`verifyChecksum`)  
- `#FilenameUrl` is out of the destination repo, the published index is standard  
- `Packages` is compressed to `.gz` and `.xz`, and on demand the stanzas are republished verbatim so `Conflicts`/`Replaces`/`Multi-Arch` survive  
- `Release` is generated with `MD5Sum`/`SHA256`, so `apt update` accepts the repo  
- parallel downloads and `Pre-Depends` resolution  

---

### 📝 TODO
- verify the **mirror's** `Release` signature, so a rewritten index is caught instead of trusted  
- sign **our own** `Release` with GPG and publish a real `InRelease`  
- improve cache (mini db), optimize index creation  
- separate cache paths from destination repo  
- pick the highest version when a package appears more than once in an index  
- honour version constraints instead of dropping them  
- pacman delta packages, and `.db.sig`  
- serve a `206` for a `Range` on an on-demand miss, instead of the whole file  

---


### 💸 Sponsors
  [![PayPal](https://img.shields.io/badge/PayPal-00457C?style=for-the-badge&logo=paypal&logoColor=white)](https://paypal.me/osnieltux)
