# Releasing `ab0t-quota-go`

This is a public library — services import it to get quota / rate-limit / tier
enforcement and (optionally) durable paid-billing. Cutting a release is a gated,
repeatable procedure. **Read this before you tag or push a version.**

## TL;DR

```bash
# 1. Pick the version (git-tag only — there is no version file to edit).
#    Tag format is mandatory vMAJOR.MINOR.PATCH (or vX.Y.Z-rcN).

# 2. Run the pre-release gate (does NOT push or tag — safe to run anytime):
make release VERSION=vX.Y.Z

# 3. Secret scan (also part of the gate; run standalone if you want):
make scan

# 4. Commit your work (the gate operates on a clean tree).

# 5. Tag + push — the deliberate operator step (see "Publishing" below):
git tag -a vX.Y.Z -m "vX.Y.Z: <one-line change summary>"
git push origin main --follow-tags
```

## The version lives in the tag, not a file

`make release` derives the build version from `git describe --tags` — there is
**no `version` constant to bump.** `vX.Y.Z` becomes real the moment you create
the annotated tag. `release/vX.Y.Z/` holds the notes for each cut.

## What `make release` checks (the gate)

`make release` refuses to proceed if any of these fail:

1. **vet** — `go vet ./...` + `gofmt -l` (unformatted files fail).
2. **test** — full `go test ./...`, including integration tests. All must pass.
3. **build** — the static binary builds, version-stamped from git.
4. **scan** — `gitleaks` over the tree; **aborts on any hit.** Install `gitleaks`
   first. `make scan-staged` runs the staged-only variant the pre-commit hook uses.

It **stages** the release artifacts under `release/` and **stops there** — it does
**not** commit, tag, or push. Publishing is always a deliberate human step.

## Version policy (semver)

- **PATCH** (`v0.1.3 → v0.1.4`) — bug fixes, internal changes, nothing a caller
  must react to.
- **MINOR** (`v0.1.x → v0.2.0`) — **any breaking change**: a new required config
  field, a changed default that can make a mis-configured service refuse to start,
  a required parameter added, a changed error contract. If a caller must do
  something to keep working, it is at least a MINOR — never a PATCH.
- Tag the commit summary with `BREAKING:` when it is one.

> **Why this matters:** a caller pinned to `~= v0.1.0` auto-adopts the next PATCH.
> Shipping a breaking change as a PATCH breaks them with no warning. If the change
> alters what a caller must provide or how it must be configured, bump the MINOR.

## When NOT to release

- `go test ./...` is skipping or not actually running the suite — a green from
  zero tests is not green.
- You are adding a required config field or changing a startup gate (a service
  that used to boot now refuses to). That is **breaking** — MINOR, not PATCH.
- Your changes touch files that belong in a consumer's own repo, not this library.
  This repo is public; consumer-specific content does not live here.

## Publishing (the operator step the gate never does)

1. Confirm the pre-release gate is green (`make release VERSION=vX.Y.Z`).
2. Confirm the tree is committed and the secret scan passed.
3. `git tag -a vX.Y.Z -m "…"` then `git push origin main --follow-tags`.
4. Confirm the tag landed:
   `https://github.com/ab0t-com/ab0t-quota-go/releases/tag/vX.Y.Z`
5. Tell consumers the new version is available. **Do not assume they auto-adopt** —
   a `go get` bump is theirs to make.

## Recovery from a half-pushed state

- Tag exists locally, not on remote → `git push origin vX.Y.Z`.
- Tag exists on remote at the wrong commit → that version is **burned**; bump to
  the next PATCH and re-cut. Never move a published tag.

## Files involved

| File | Purpose |
|---|---|
| `Makefile` | `make release` / `make scan` — the gate |
| `scripts/release.sh` | Pre-release staging (no push/tag) |
| `scripts/scan.sh` | gitleaks wrapper (`--staged` for the hook) |
| `release/vX.Y.Z/` | Per-release notes |

The Python sibling `ab0t-quota` releases through `scripts/push.sh` (which also
pushes); this Go repo keeps tag+push as a separate manual step by design.
