# juliaup-mirror

Build a mirror of the [juliaup](https://github.com/JuliaLang/juliaup) server, so people can install and update Julia with juliaup behind a firewall or on an air-gapped network.

`juliaup-mirror sync` downloads the channels and platforms you pick into a directory. Serve that directory with any static web server (nginx, S3, an internal file share), point clients at it with `JULIAUP_SERVER`, and `juliaup add`, `juliaup update` and `juliaup self update` work as usual.

## Quick start

```sh
go install github.com/thelonewolf1603/juliaup-mirror@latest

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

Serve the mirror over **https**: juliaup accepts plain `http://` only for localhost, and `install.sh` downloads over https only. A certificate from an internal CA works if the CA is in the client's system trust store. For a quick setup or a test, `juliaup-mirror serve -dir /srv/julia -addr :443 -tls-cert cert.pem -tls-key key.pem` serves the directory itself.

## Choosing what to mirror

| Flag | Default | |
|---|---|---|
| `-channels` | `release,lts` | Channel names or globs: `1.11`, `1.10.*`, `beta`. `*` mirrors everything (several hundred GB). |
| `-targets` | Linux x86_64/aarch64 (musl and gnu), macOS x86_64/aarch64, Windows x86_64 (gnu and msvc) | juliaup target triples. |
| `-arch-variants` | off | Also mirror the `~x86` / `~x64` variants of each channel (e.g. 32-bit Julia on a 64-bit host). |
| `-juliaup-version` | upstream's current release | The juliaup release clients install and self-update to. |
| `-macos-dmg` | off | Also mirror the `.dmg` juliaup tries before the `.tar.gz` on macOS. |
| `-metadata-only` | off | Skip Julia archives, e.g. when a caching proxy in front of the mirror fetches `bin/` from upstream. |

On Linux, juliaup's install script always installs the **musl** juliaup, which reads the `*-linux-musl` version database. Mirror both `-linux-musl` and `-linux-gnu` targets; they point at the same Julia archives, so the musl target costs only a few MB. Likewise on Windows, the MSI installs the `-pc-windows-gnu` juliaup and the Store build is `-pc-windows-msvc`.

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

so the mirror always sorts above the upstream version it came from, and every change on the mirror gets a higher number than the last. The next number is worked out from what is already published, not only from `.juliaup-mirror-state.json`, so a killed sync or a lost state file never republishes a number with different content.

A mirror can be the `-upstream` of another one (say, an internet-facing mirror feeding an air-gapped one). Each level multiplies by 1000 again (`1.0.104006` → `1.0.104006000`), which leaves room for a few levels.

**Side effect:** mirror numbers sort above any realistic official number, so a client that stops using the mirror keeps the mirror's channel list. Delete `~/.julia/juliaup/versiondb-*.json` on that client after unsetting `JULIAUP_SERVER`.

### Pruning

The current and the previous version databases are kept; older ones are removed. With `-prune`, Julia archives and juliaup binaries that neither of those two lists are deleted too, so a client that fetched the previous database a moment ago can still install from it.

A sync holds a lock on the mirror directory, so overlapping cron runs fail fast instead of racing, and it removes partial downloads a killed run left behind. `-server-url` is remembered, so later runs keep `install.sh` up to date.

## Not supported (yet)

- Nightly and PR builds (`nightly`, `pr1234`). juliaup fetches them from separate URLs that this tool does not mirror.
- A Windows install script. Windows users can set `JULIAUP_SERVER` and use the mirrored MSI from `juliaup/winmsi/`.
- Signature or checksum checks of downloaded archives.

## Development

```sh
go test ./...
go vet ./...
```

## License

MIT, see [LICENSE](LICENSE).
