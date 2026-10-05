# Contributing to BranchHarbor

Thanks for helping improve BranchHarbor. The project favors **small, reviewable changes**, explicit invariants, and reproducible tests over feature count.

## Before you start

1. Use Linux, or WSL2 with the repository stored on its Linux filesystem.
2. Install the Go version pinned in [`.go-version`](./.go-version).
3. Read [README.md](./README.md), [API.md](./API.md), and [SECURITY.md](./SECURITY.md) for the behavior and trust boundary you are changing.
4. Reproduce bugs with a test before fixing them whenever practical.

## Verify locally

```bash
make verify
make fuzz
```

Focused commands are also available:

```bash
make test
make integration
make acceptance
make security
make race
```

## Change expectations

| Change | Expected verification |
|---|---|
| Documentation only | Links, commands, and examples checked |
| Ordinary implementation | Unit + relevant integration tests |
| HTTP behavior | API / integration + acceptance tests |
| Authorization | Security / negative-boundary tests |
| Persistence / recovery | Restart or fault-path tests |
| Parser / merge logic | Existing fuzz target or additional focused fuzzing |

> [!IMPORTANT]
> Do not delete, skip, weaken, or rewrite a meaningful test simply because the implementation fails it.

## Repository hygiene

Use synthetic local data only. Never commit:

- credentials, tokens, or store keys;
- real store directories;
- employer-confidential material;
- personal or customer data;
- fabricated test or benchmark output.

## Pull requests

Keep PRs focused. Explain:

- what changed;
- which invariant or behavior is affected;
- what you ran;
- what could regress;
- whether meaningful AI assistance was used.

A coding agent may help produce a change, but the contributor should understand and be able to defend the final code.

## Security reports

Do not open a public issue for a security vulnerability that would expose an exploit or sensitive material. Follow [SECURITY.md](./SECURITY.md).

---

<p align="center"><sub>Focused diff · explicit invariant · reproducible verification.</sub></p>
