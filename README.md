# juliaup-mirror

Build a mirror of the [juliaup](https://github.com/JuliaLang/juliaup) server, so people can install and update Julia with juliaup behind a firewall or on an air-gapped network.

`juliaup-mirror sync` downloads the channels and platforms you pick into a directory. Serve that directory with any static web server (nginx, S3, an internal file share), point clients at it with `JULIAUP_SERVER`, and `juliaup add`, `juliaup update` and `juliaup self update` work as usual.

## Quick start

```sh
go install github.com/juliaup-mirror/juliaup-mirror@latest

# See what a mirror of release + lts would download
juliaup-mirror sync -out /srv/julia -channels release,lts -dry-run

# Build it, with an install script that points at the mirror
juliaup-mirror sync -out /srv/julia -channels release,lts \
  -server-url https://julia.example.com

# Re-run on a schedule (cron, systemd timer) to pick up new releases
juliaup-mirror sync -out /srv/julia -channels release,lts \
  -server-url https://julia.example.com -prune
```

Serve `/srv/julia` at `https://julia.example.com`. Users then install with:

```sh
curl -fsSL https://julia.example.com/install.sh | sh
```

or, with juliaup already installed:

```sh
export JULIAUP_SERVER=https://julia.example.com
juliaup add lts
```

`JULIAUP_SERVER` has to stay set (shell profile, `/etc/profile.d`, or similar) or juliaup goes back to the official server.

juliaup only accepts `https://` servers, except `http://localhost` and `http://127.0.0.1`. `juliaup-mirror serve -dir /srv/julia` serves a mirror on localhost for testing.

## Choosing what to mirror

| Flag | Default | |
|---|---|---|
| `-channels` | `release,lts` | Channel names or globs: `1.11`, `1.10.*`, `beta`. `*` mirrors everything (several hundred GB). |
| `-targets` | Linux x86_64/aarch64 (musl and gnu), macOS x86_64/aarch64, Windows x86_64 | juliaup target triples. |
| `-arch-variants` | off | Also mirror the `~x86` / `~x64` variants of each channel (e.g. 32-bit Julia on a 64-bit host). |
| `-juliaup-version` | upstream's current release | The juliaup release clients install and self-update to. |
| `-macos-dmg` | off | Also mirror the `.dmg` juliaup tries before the `.tar.gz` on macOS. |
| `-metadata-only` | off | Skip Julia archives, e.g. when a caching proxy in front of the mirror fetches `bin/` from upstream. |

On Linux, juliaup's install script always installs the **musl** juliaup, which reads the `*-linux-musl` version database. Mirror both `-linux-musl` and `-linux-gnu` targets; they point at the same Julia archives, so the musl target costs only a few MB.

## How it works

A juliaup client reads these paths relative to `JULIAUP_SERVER`:

```
juliaup/RELEASECHANNELDBVERSION                     version of the version database
juliaup/versiondb/versiondb-<version>-<target>.json channels -> Julia versions -> download path
bin/...                                             Julia archives
juliaup/RELEASECHANNELVERSION                       juliaup version, for self update
juliaup/bin/juliaup-<version>-<target>.tar.gz       juliaup itself
```

`sync` fetches the upstream version databases, keeps only the selected channels and the Julia versions they point to, downloads those archives, and then writes the filtered databases. The version pointers are written last, so clients never see a database whose files are still downloading. A failed download leaves the previous mirror in place.

### Version numbers

juliaup only downloads a version database whose version is **higher** than the one it already has (built-in or cached). A filtered database therefore can't reuse the upstream number: clients that have already seen upstream's full database would never pick up the filtered one. The mirror publishes

```
upstream 1.0.104  ->  mirror 1.0.104000, 1.0.104001, ...   (one step per change of selection)
upstream 1.0.105  ->  mirror 1.0.105000
```

so the mirror always sorts above the upstream version it came from and below the next one. State is kept in `.juliaup-mirror-state.json` in the mirror directory.

One side effect: a client that has used the mirror and then switches back to the official server keeps the mirror's database, because the official version number is lower. Delete `~/.julia/juliaup/versiondb-*.json` on that client to fix it.

### Pruning

Old version databases are always removed once they are two versions old. With `-prune`, Julia archives and juliaup binaries that the current selection no longer lists are deleted too.

## Not supported (yet)

- Nightly and PR builds (`nightly`, `pr1234`). juliaup fetches them from separate URLs that this tool does not mirror.
- A Windows install script. Windows users can set `JULIAUP_SERVER` and use the mirrored MSI from `juliaup/winmsi/`.
- Signature or checksum checks of downloaded archives.

## Development

```sh
go test ./...
go vet ./...
```
