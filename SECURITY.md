# Security policy

<p align="center">
  <strong>Explicit capability scope · local trusted operator · documented residual risk</strong>
</p>

BranchHarbor is experimental software for a trusted operator on a local Linux filesystem. It does **not** claim to be a production service, hostile-process sandbox, or public-Internet endpoint.

> [!WARNING]
> Keep BranchHarbor bound to loopback. The binary provides no TLS. For remote access, use a trusted SSH tunnel rather than exposing the service directly.

## Reporting a vulnerability

Use this repository's **Security → Report a vulnerability** flow for private disclosure.

Please include:

- the affected revision;
- a minimal synthetic reproducer;
- expected versus observed behavior;
- impact and preconditions, if known.

Do **not** include live credentials, private content, personal information, or exploit traffic against third parties. Do not publish unresolved vulnerability details in a public issue.

## Trust boundary

| Area | Current assumption |
|---|---|
| Local OS / directory owner | Trusted |
| Service process | Trusted |
| HTTP clients | Untrusted |
| Stored file contents | Untrusted |
| Network exposure | Loopback / trusted SSH tunnel |
| Multi-node operation | Unsupported |
| Public Internet | Unsupported |

BranchHarbor does not execute stored files and does not attempt to isolate hostile local processes.

## Capability tokens

Tokens are locally HMAC-authenticated capabilities, **not OAuth or JWTs**.

An operator issues scopes covering:

- branch;
- optional path prefix;
- read / write operations;
- admin authority where explicitly granted.

Bearer theft grants the token's remaining authority. Token lifetime is capped at one hour, and immediate per-token revocation is unavailable.

Store keys and token files in owner-only directories with mode `0600`.

If a key is compromised:

1. stop the service;
2. rotate the store key;
3. reissue required tokens;
4. verify that old tokens fail.

Key rotation invalidates all previously issued tokens.

## Data handling

Do not commit or publish:

- `auth.key`;
- token files;
- live store directories;
- personal or confidential content;
- credentials or secrets in fixtures.

The service does not intentionally log tokens or stored file contents.

BranchHarbor does **not** provide:

- encryption at rest;
- secure erasure of historical secrets;
- protection from a malicious local root/filesystem owner;
- semantic safety analysis of proposed file content.

Garbage collection removes only unreachable orphan objects; it is not secret erasure.

## Corruption and recovery

Preserve a damaged store before investigation.

Prefer restoring a verified, stopped-store backup rather than manually trimming complete corrupt frames to force startup.

Known residual limitations include:

- Format 1 partial-frame ambiguity;
- exact frame-boundary suffix removal;
- coherent history rewrites without an external witness;
- process-crash testing that does not certify physical power-loss durability.

These constraints are also summarized in [README.md](./README.md).

## CI and contribution hygiene

Security-sensitive changes should include negative-boundary tests. Do not weaken a permission, parsing, recovery, or acceptance assertion simply to make a failing implementation pass.

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the contribution workflow and [API.md](./API.md) for request limits and authority rules.

---

<p align="center"><sub>Bound authority explicitly. Preserve evidence. State residual risk.</sub></p>
