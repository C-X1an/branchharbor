# BranchHarbor HTTP API

<p align="center">
  <strong>Small JSON-over-HTTP surface · loopback by default · capability-scoped clients</strong>
</p>

The service accepts JSON over loopback HTTP. Send `Authorization: Bearer TOKEN` except for `/healthz`, and `Content-Type: application/json` for POST requests.

> [!IMPORTANT]
> Read bearer tokens from their private files. Do not embed them in source, shell history, URLs, or logs. BranchHarbor provides no TLS; keep it on loopback or place it behind a trusted SSH tunnel.

## Conventions

A branch head is:

```json
{"commit":"64 lowercase hexadecimal characters","version":1}
```

Successful mutations return:

```json
{"head":{"commit":"...","version":2},"replayed":false}
```

A replay returns the **original mutation result**, which may differ from the branch's current head.

The parser rejects unknown or duplicate JSON fields, trailing values, invalid UTF-8, nesting beyond 32, and multi-valued query parameters.

## Endpoints

| Method | Route | Result | Required authority |
|---|---|---|---|
| `GET` | `/healthz` | `{"ready":true}`; 503 when poisoned | Public |
| `GET` | `/v1/head?branch=main` | Head | Read on branch |
| `GET` | `/v1/files?branch=main` | Head + visible file metadata | Read; filtered by prefix |
| `GET` | `/v1/file?branch=main&path=docs/a.txt` | Head + base64 bytes | Read on branch/path |
| `GET` | `/v1/branches` | All branch heads | Admin |
| `POST` | `/v1/commit` | Mutation result | Write on every changed path |
| `POST` | `/v1/fork` | Mutation result | Admin |
| `POST` | `/v1/merge/preview` | Base / target / source + conflicts | Admin |
| `POST` | `/v1/merge` | Mutation result | Admin |
| `POST` | `/v1/restore` | Mutation result | Admin |
| `GET` | `/metrics` | Aggregate requests / errors / mutations | Admin |

## Commit example

Replace the expected head with the head you actually observed:

```json
{
  "branch": "agent-a",
  "expected": {
    "commit": "<observed digest>",
    "version": 1
  },
  "request_id": "change-001",
  "puts": {
    "docs/a.txt": "SGVsbG8="
  },
  "deletes": [
    "docs/old.txt"
  ]
}
```

`puts` contains base64-encoded bytes. An empty string means an empty file, not deletion. A request must contain at least one modification.

The service rejects duplicate deletes, put/delete overlap, file/directory collisions, stale expected heads, and whole-file or ambiguous-ancestry merge conflicts.

## Retry and idempotency semantics

Retry identity is:

```text
authenticated subject + request_id
```

After a timeout or lost response:

1. resend the **identical** operation;
2. reuse the same request ID;
3. BranchHarbor returns the original mutation result when the request matches.

Changing contents or expected heads while reusing the same identity produces `idempotency_conflict`.

Authorization is checked on **every** request, including replays. Back off on `429` or `503`; do not retry in a tight loop.

## Branch and path rules

Branch names are 1–64 ASCII alphanumeric, underscore, or hyphen characters.

Logical paths are 1–240 bytes of slash-separated non-empty segments containing ASCII alphanumeric characters, dot, underscore, or hyphen.

Rejected forms include:

- absolute paths;
- whole `.` or `..` segments;
- backslashes;
- percent signs;
- empty segments.

A directory capability prefix is empty for all paths or a valid logical path ending in `/`. For example, `docs/` does **not** authorize `docs-private/`.

Only admin tokens may use the wildcard branch. Ordinary clients cannot fork, promote, restore, or list every branch. Permission denial happens before state lookup.

## Errors

Errors use this envelope:

```json
{
  "error": {
    "code": "CODE",
    "message": "MESSAGE"
  }
}
```

| HTTP | Codes |
|---:|---|
| 400 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `forbidden` |
| 404 | `not_found` |
| 409 | `conflict`, `idempotency_conflict`, `ambiguous_base` |
| 413 | `limit` |
| 429 | `busy` |
| 503 | `unavailable` |
| 500 | `internal` |

Unknown routes return 404; wrong methods return 405. Responses disable caching.

## Default resource bounds

| Resource | Default bound |
|---|---:|
| Individual file | 4 MiB |
| Snapshot paths | 2,048 |
| Snapshot bytes | 64 MiB |
| Modifications / request | 256 |
| Request body | 8 MiB |
| Branches | 64 |
| Objects + journal | 256 MiB |
| Journal | 32 MiB |
| Journal frame | 1 MiB |
| Headers | 16 KiB |
| Concurrent requests | 16 |

These are **input and resource bounds**, not measured capacity guarantees.

## Client example

Use [`examples/agent_client.py`](./examples/agent_client.py) for the standard-library client.

For trust assumptions, token handling, and residual limitations, read [SECURITY.md](./SECURITY.md).

---

<p align="center"><sub>Back to the <a href="./README.md">project overview</a>.</sub></p>
