# BranchHarbor

BranchHarbor is an experimental single-node versioned workspace store for
cooperating tool clients. Clients submit bounded file changes on isolated
branches. An operator reviews and promotes changes through whole-file three-way
merges. The service provides immutable content-addressed objects, atomic commits,
expected-head checks, durable retry identity and branch/path capabilities.

It does not execute stored files or call an AI model. Linux on a local filesystem
with file and directory `fsync` support is required. Windows users need WSL2 and
must keep stores inside its Linux filesystem, rather than a Windows or network
mount. Native Windows/macOS, shared network filesystems and distributed operation
are unsupported. Physical power-loss guarantees and production capacity are
unverified.

## Build and try

Install Go 1.27.1 (the `.go-version` pin), Make, Python 3.11+ and a C compiler for
the race detector. The runtime and Python client use only standard libraries.

```sh
make build
make demo
```

The demo runs a real temporary loopback service with two scoped clients. It checks
branch isolation, permission denials, disjoint merges, stale writes, conflicting
merges and retry identity across restart. It removes its temporary store and
requires no credentials, cloud account or model service.

For a persistent local store:

```sh
./bin/branchharbor init --dir .bh
./bin/branchharbor token --dir .bh --subject operator --branch '*' \
  --ops admin --ttl 900 --out operator.token
./bin/branchharbor serve --dir .bh --listen 127.0.0.1:8787
```

The token command writes a new owner-only file and does not print its value.
Operator tokens have full store authority. Issue branch and directory-prefix
tokens with `--ops read,write` for ordinary clients. See [API.md](API.md) and
[examples/agent_client.py](examples/agent_client.py) for client usage.

## Verify

```sh
make verify
make fuzz
```

`make verify` checks the supported toolchain, Go formatting, build, vet, tests,
race detection and the real HTTP demo. `make fuzz` runs five bounded parser and
merge campaigns. `make test`, `make integration`, `make acceptance` and `make
security` run subsets directly. `make clean` removes the build output only.

## Operate safely

Keep the service bound to loopback. For remote access use an SSH tunnel, for
example `ssh -L 8787:127.0.0.1:8787 operator@HOST`; the binary does not provide TLS.
The service rejects browser Origin requests. One process owns a store, while
multiple authenticated HTTP clients may use that process.

A successful mutation synchronizes object files and their directory entries
before synchronizing the journal reference. Uncertain journal I/O disables
mutations until restart. An error or lost response may follow a committed write:
resolve it with the identical request ID and payload. Complete detectable
corruption causes startup refusal. Preserve the original store and restore a
verified backup; never manually trim corrupt frames to force startup.

Back up only after stopping the server. Copy the entire directory, including
`FORMAT`, journal, objects and `auth.key`, preserving an owner-only directory and
0600 key/files. Run `./bin/branchharbor inspect --dir BACKUP` and compare heads
before serving the copy. A copied key preserves existing token authority.
Unsupported formats are refused; there is no automatic migration. Roll back a
binary only when it supports the store's format.

History is retained and orphan GC is not secret erasure. Journal compaction is
not implemented. Format 1 has an unauthenticated length header: damaged bytes
identical to a genuine partial writer frame cannot be distinguished from an
interrupted write. Checksums and hash chains cannot detect coherent full-history
rewrites or exact frame-boundary suffix removal without an external witness.
Process-crash tests do not certify physical storage durability.

Read [SECURITY.md](SECURITY.md) before using the service. Original code and
documentation are [MIT-licensed](LICENSE).
