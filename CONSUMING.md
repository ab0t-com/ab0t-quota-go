# Consuming ab0t-quota-go

Two ways to use this project. Most teams want path A; ops teams + sandbox
operators want path B.

---

## Path A — Go module import (no binary)

This is how Go libraries work. There is no install step; you import the
package and `go build` fetches it.

### 1. Pin a version in your go.mod

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.1.0
```

Or pin to a specific commit:

```bash
go get github.com/ab0t-com/ab0t-quota-go@a1b2c3d
```

Or to the latest tagged release:

```bash
go get github.com/ab0t-com/ab0t-quota-go@latest
```

### 2. Import in your code

```go
import (
    "github.com/ab0t-com/ab0t-quota-go/quota"
    "github.com/ab0t-com/ab0t-quota-go/authevents"
)

func main() {
    q, err := quota.Setup(ctx, quota.Options{
        ConfigPath: "quota-config.json",
    })
    if err != nil {
        log.Fatal(err)
    }
    defer q.Close(context.Background())

    http.Handle("/api/", q.Middleware(deps)(yourHandler))
}
```

### How the Go module proxy + GitHub work together

When you run `go get`, the Go toolchain does this:

1. Reads `github.com/ab0t-com/ab0t-quota-go` as `<host>/<owner>/<repo>`.
2. Asks the public Go module proxy (`proxy.golang.org`) for that version.
3. The proxy reads the GitHub repo at the corresponding tag and serves
   it as a zip + go.mod + checksum.
4. Your local `go.sum` records the checksum so a later compromise of the
   repo doesn't go unnoticed.

This means **you do not need GitHub auth for public modules** (anyone can
`go get` it). It means **the release is the git tag**: pushing a
`v1.2.3` tag is the publish event — there is no separate "publish to
package manager" step. It also means **the source IS the artifact** —
the proxy just packages and serves what's in the repo.

### Private / internal Go modules

If this repo were private:

```bash
export GOPRIVATE="github.com/ab0t-com/*"
export GONOSUMCHECK="off"   # only if you accept the supply-chain tradeoff
git config --global url."git@github.com:".insteadOf "https://github.com/"
```

Then `go get` will fetch through `git` over SSH instead of the public
proxy. The repo is currently public so this isn't needed.

### Version selection rules ("minimum version selection")

When two of your dependencies require this lib, Go picks the **higher**
of the two requested versions — never the lower, never the highest
available. To force a newer version than the modules ask for, run
`go get github.com/ab0t-com/ab0t-quota-go@v0.2.0` in your top-level
module and commit the resulting `go.mod`/`go.sum`.

---

## Path B — quotactl CLI binary

`quotactl` is the admin CLI (replay events, query the ledger, run
auto-subscribe by hand, print Capabilities, etc.). It is shipped as a
single static binary per (OS, arch).

### Option 1 — `go install`

If you have a Go toolchain:

```bash
go install github.com/ab0t-com/ab0t-quota-go/cmd/quotactl@v0.1.0
```

This drops a `quotactl` binary into `$GOBIN` (or `$GOPATH/bin`). It
builds from source on your machine — no GitHub release needed.

### Option 2 — Download a prebuilt release binary

If you don't want a Go toolchain (the common ops scenario):

```bash
VERSION=v0.1.0
ARCH=amd64                 # or arm64
OS=$(uname | tr A-Z a-z)    # darwin | linux

curl -L -o quotactl \
  https://github.com/ab0t-com/ab0t-quota-go/releases/download/${VERSION}/quotactl-${OS}-${ARCH}

# Verify
curl -L https://github.com/ab0t-com/ab0t-quota-go/releases/download/${VERSION}/SHA256SUMS \
  | grep "quotactl-${OS}-${ARCH}$" \
  | shasum -a 256 -c -

chmod +x quotactl
sudo mv quotactl /usr/local/bin/
quotactl --version
```

Windows users: download `quotactl-windows-amd64.exe` directly.

### Subcommands

```
quotactl subscribe-events    # idempotent register with auth
quotactl events --user UID   # tail the ledger
quotactl replay --file EVENTS.jsonl --target URL --secret HMAC
quotactl backfill --input USERS.csv --target URL --secret HMAC
quotactl delete-user --user-id UID    # GDPR
quotactl capabilities --config quota-config.json
```

Every subcommand has `--help`. `quotactl --version` prints the linked
build version + commit + UTC build time.

---

## Building it yourself

For a hermetic build from this repo:

```bash
make dist         # cross-compile all 5 (os, arch) pairs → release/<version>/
make release VERSION=v0.1.0   # vet + race tests + dist + notes scaffold
```

`scripts/build.sh` is the underlying script — see it for the exact
ldflags and target matrix.

---

## What ships and what doesn't

| Artifact                          | Distribution                | Path |
|-----------------------------------|-----------------------------|------|
| `quota`, `engine`, `middleware`,  | source via `go get`          | A    |
| `authevents`, `handlerledger`,    |                              |      |
| `counters`, `providers`, etc.     |                              |      |
| `quotactl` CLI                    | `go install` or GH releases  | B    |
| Example servers (`examples/*`)    | source only (read + adapt)   | —    |
| Container images                  | not shipped (lib, not service) | — |

The library packages are **API-stable per semver**. Breaking changes go
in a new major version; everything else respects backwards-compat. The
quotactl CLI follows the same versioning.
