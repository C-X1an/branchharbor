# HTTP API

The service accepts JSON over loopback HTTP. Send `Authorization: Bearer TOKEN`
except for `/healthz`, and `Content-Type: application/json` for POST requests.
Read the token from its private file; do not embed it in
source or logs. There is no TLS, browser access or external object-by-digest
endpoint. Unknown or duplicate JSON fields, trailing values, invalid UTF-8 and
nesting beyond 32 are rejected. Query parameters must be single-valued.

A head is `{"commit":"64 lowercase hexadecimal characters","version":1}`.
Successful operations return HTTP 200. Mutation results are
`{"head":HEAD,"replayed":false}`. A replay returns the original result, which
may differ from the branch's current head.

| Method and route | Request | Result | Authority |
|---|---|---|---|
| GET /healthz | None | `{"ready":true}`; 503 when poisoned | Public |
| GET /v1/head?branch=main | Branch query | Head | Read on branch |
| GET /v1/files?branch=main | Branch query | `{head,files:[{path,digest,size}]}` | Read; entries filtered by prefix |
| GET /v1/file?branch=main&path=docs/a.txt | Branch/path queries | `{head,data:"base64"}` | Read on branch/path |
| GET /v1/branches | None | `{branches:{name:HEAD}}` | Admin |
| POST /v1/commit | Commit request below | Mutation result | Write on every changed path |
| POST /v1/fork | `{branch,source,expected:HEAD,request_id}` | Mutation result, version 1 | Admin |
| POST /v1/merge/preview | `{target,source}` | `{base,target:HEAD,source:HEAD,conflicts:[path]}` | Admin |
| POST /v1/merge | `{target,source,expected_target:HEAD,expected_source:HEAD,request_id}` | Mutation result | Admin |
| POST /v1/restore | `{branch,commit,expected:HEAD,request_id}` | Mutation result; adds history | Admin; commit must be reachable from branch |
| GET /metrics | None | Aggregate requests/errors/mutations | Admin |

Commit example (replace the expected head with the observed head):

```json
{
  "branch": "agent-a",
  "expected": {"commit": "<observed digest>", "version": 1},
  "request_id": "change-001",
  "puts": {"docs/a.txt": "SGVsbG8="},
  "deletes": ["docs/old.txt"]
}
```

Puts contain base64 bytes; an empty string is an empty file, not deletion. A
request must contain modifications. Duplicate deletes, put/delete overlap and
file/directory collisions are rejected. Expected commit and version are checked
together. A merge requires both the reviewed target and source heads; whole-file
or ambiguous-ancestry conflicts are refused without changing the target.

Retry identity is the authenticated subject plus request ID. Repeat the identical
operation after a timeout or lost response. Changed contents or expected heads
with the same identity produce `idempotency_conflict`. A conflict requires
intentional reconciliation and a new request ID. Identical replay is checked
before comparing the current head; authorization is checked on every request.
Back off on 429/503 rather than repeatedly flooding the service.

Branch names are 1–64 ASCII alphanumeric, underscore or hyphen characters. Paths
are 1–240 bytes of slash-separated nonempty ASCII alphanumeric, dot, underscore
or hyphen segments. Absolute paths, whole `.`/`..` segments, backslashes and
percent signs are rejected. A directory prefix is empty for all paths, or a valid
logical path ending in `/`: `docs/` does not authorize `docs-private/`. Only admin
tokens may use the wildcard branch. Ordinary clients cannot fork, promote,
restore or list all branches. Permission denial precedes state lookup.

Errors are `{"error":{"code":"CODE","message":"MESSAGE"}}`:

| Status | Codes |
|---|---|
| 400 | invalid |
| 401 | unauthenticated |
| 403 | forbidden |
| 404 | not_found |
| 409 | conflict, idempotency_conflict, ambiguous_base |
| 413 | limit |
| 429 | busy |
| 503 | unavailable |
| 500 | internal |

Unknown routes return 404 and wrong methods return 405. Responses disable
caching. Defaults bound files to 4 MiB, snapshots to 2048 paths/64 MiB, requests to
256 modifications/8 MiB bodies, branches to 64, objects plus journal to 256 MiB,
journal to 32 MiB, frames to 1 MiB, headers to 16 KiB and concurrent requests to
16. These are input/resource bounds, not measured capacity guarantees.

Use [examples/agent_client.py](examples/agent_client.py) for the standard-library
client. [SECURITY.md](SECURITY.md) describes token and trust limits.
