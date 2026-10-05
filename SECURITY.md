# Security

BranchHarbor is experimental software for a trusted operator on a local Linux
filesystem. Keep it bound to loopback and use an SSH tunnel for remote access.
The binary has no TLS and rejects browser Origin requests. It does not execute
stored files, isolate hostile local processes or provide a production service.

Tokens are local HMAC-authenticated capabilities, not OAuth or JWT. An operator
issues branch/prefix/read/write scopes; admin tokens have full store authority.
Bearer theft grants the remaining token authority. Lifetimes are capped at one
hour, and immediate per-token revocation is unavailable. Store keys and token
files in owner-only directories with mode 0600. Rotate a compromised key while
the service is stopped, then reissue tokens; rotation invalidates all old tokens.

Do not commit keys, token files, real stores, personal content or confidential
data. The service does not log tokens or file contents. There is no encryption
at rest or secure erasure of historical secrets. Garbage collection removes
only unreachable orphan objects. The local OS and directory owner are trusted.
Whole-file merge does not assess whether a proposal's contents are safe.

Preserve damaged stores before investigation. Restore a verified stopped-store
backup rather than trimming complete corrupt frames. Format-1 partial-frame
ambiguity, exact-boundary truncation and coherent rewrites remain limitations;
see [README.md](README.md). Process-crash checks do not prove physical power-loss
durability, production availability or capacity.

Report a vulnerability privately using this repository's **Security → Report a
vulnerability** feature. Include the affected revision, a synthetic reproducer
and expected versus observed behavior. Do not submit live credentials, private
content or third-party exploit traffic, and do not post unresolved reports as
public issues.
