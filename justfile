# justfile for parts-finder-mcp
# Run `just` to see all available commands.

set shell := ["bash", "-euo", "pipefail", "-c"]

# Default — list recipes.
default:
    @just --list --unsorted

# ─────────────────────────── Build & Test ───────────────────────────

# Version baked into the binary at link time.
GO_LDFLAGS := "-X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)"

# Build the binary into ./bin/.
build:
    mkdir -p bin
    go build -ldflags="{{GO_LDFLAGS}}" -o bin/parts-finder .
    @echo "Built ./bin/parts-finder"

# Install into $GOBIN (or $GOPATH/bin).
install:
    go install -ldflags="{{GO_LDFLAGS}}" .
    @echo "Installed parts-finder to $(go env GOBIN || echo $(go env GOPATH)/bin)"

# Rebuild the binary the registered MCP server points at.
refresh-mcp:
    go build -ldflags="{{GO_LDFLAGS}}" -o parts-finder .
    @echo "Refreshed ./parts-finder (registered MCP binary)"

fmt:
    gofmt -w .

# Format check + vet.
lint: fmt
    go vet ./...

# Strict read-only check — same logic CI runs, exposed for local pre-push
# verification. Fails if formatting would change or vet fires.
lint-check:
    #!/usr/bin/env bash
    set -euo pipefail
    unformatted=$(gofmt -l .)
    if [ -n "$unformatted" ]; then
        echo "code is not formatted; run 'just fmt':"
        printf '%s\n' "$unformatted"
        exit 1
    fi
    go vet ./...

test:
    go test ./...

check: lint test sync-flake

clean:
    rm -rf bin/

# ─────────────────────────── Nix ───────────────────────────

nix-build:
    nix build .#parts-finder

nix-check:
    nix flake check --print-build-logs

# Keep flake.nix's `vendorHash` aligned with the current go.sum.
#
# A sha256 of go.sum is embedded as a `# go-sum:` line in flake.nix.
# When the cached digest matches go.sum on disk, sync-flake returns
# immediately without running `nix build`. That makes it cheap
# enough to run on every `just check`, so a dev `go get` flow can
# never push a master commit that breaks nix CI on master.
#
# By default this does NOT touch the version string — release-only
# concern. Pass an explicit `version` argument to also rewrite
# `version = "…"` + the `-X main.version=…` ldflag (used by the
# release recipes). Pass `--force` to bypass the cache and re-run
# the nix build even if go.sum looks unchanged.
sync-flake version="":
    #!/usr/bin/env bash
    set -euo pipefail
    ARG="{{version}}"
    FORCE=0
    VERSION=""
    case "$ARG" in
        "")          ;;
        "--force")   FORCE=1 ;;
        *)           VERSION="${ARG#v}" ;;
    esac

    GO_SUM_HASH=$(sha256sum go.sum | awk '{print $1}')
    CACHED_HASH=$(awk -F': ' '/^[[:space:]]*#[[:space:]]*go-sum:/ {print $2; exit}' flake.nix | tr -d ' ')
    CURRENT_VERSION=$(awk -F'"' '/^[[:space:]]*version = "/ {print $2; exit}' flake.nix)

    NEED_HASH=0
    NEED_VERSION=0
    if [ "$FORCE" = "1" ] || [ "$GO_SUM_HASH" != "$CACHED_HASH" ]; then NEED_HASH=1; fi
    if [ -n "$VERSION" ] && [ "$VERSION" != "$CURRENT_VERSION" ]; then NEED_VERSION=1; fi

    if [ "$NEED_HASH" = "0" ] && [ "$NEED_VERSION" = "0" ]; then
        echo "sync-flake: up-to-date (go.sum=$GO_SUM_HASH version=$CURRENT_VERSION)"
        exit 0
    fi

    echo "sync-flake: refreshing (need_hash=$NEED_HASH need_version=$NEED_VERSION)"

    if [ "$NEED_HASH" = "1" ]; then
        SENTINEL="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
        sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$SENTINEL"'";|' flake.nix
        set +e
        OUT=$(nix build .#parts-finder --no-link 2>&1)
        BUILD_STATUS=$?
        set -e
        NEW_HASH=$(printf '%s\n' "$OUT" | awk '/got:[[:space:]]*sha256-/ {print $2; exit}')
        if [ -z "$NEW_HASH" ]; then
            if [ "$BUILD_STATUS" = "0" ]; then
                echo "sync-flake: unexpected nix build success with sentinel hash" >&2
                echo "$OUT" >&2
                exit 1
            fi
            echo "$OUT" >&2
            echo "sync-flake: nix build failed without printing 'got: sha256-…'" >&2
            exit 1
        fi
        sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$NEW_HASH"'";|' flake.nix
        if grep -q '^[[:space:]]*# go-sum:' flake.nix; then
            sed -i -E 's|^(\s*# go-sum:).*|\1 '"$GO_SUM_HASH"'|' flake.nix
        else
            sed -i -E 's|^(\s*vendorHash = )|          # go-sum: '"$GO_SUM_HASH"'\n\1|' flake.nix
        fi
        echo "sync-flake: vendorHash=$NEW_HASH go-sum=$GO_SUM_HASH"
    fi

    # Hard guard: refuse to leave the sentinel in flake.nix.
    if grep -q '^\s*vendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="' flake.nix; then
        echo "sync-flake: refusing to leave sentinel vendorHash in flake.nix" >&2
        exit 1
    fi

    if [ "$NEED_VERSION" = "1" ]; then
        sed -i -E 's|^(\s*version = )"[^"]*";|\1"'"$VERSION"'";|' flake.nix
        sed -i -E 's|(-X main.version=)[^"]*|\1'"$VERSION"'|' flake.nix
        echo "sync-flake: version=$VERSION"
    fi

    nix build .#parts-finder --no-link

# ─────────────────────────── Release ───────────────────────────

release-preview:
    #!/usr/bin/env bash
    set -euo pipefail
    CURRENT_TAG=$(git tag -l 'v*.*.*' --sort=-v:refname | head -1)
    CURRENT_TAG=${CURRENT_TAG:-v0.0.0}
    CURRENT_VERSION=${CURRENT_TAG#v}
    MAJOR=$(echo "$CURRENT_VERSION" | cut -d. -f1)
    MINOR=$(echo "$CURRENT_VERSION" | cut -d. -f2)
    PATCH=$(echo "$CURRENT_VERSION" | cut -d. -f3)
    echo "Current tag: $CURRENT_TAG"
    echo "  release-major: v$((MAJOR + 1)).0.0"
    echo "  release-minor: v${MAJOR}.$((MINOR + 1)).0"
    echo "  release-patch: v${MAJOR}.${MINOR}.$((PATCH + 1))"

_release-checks:
    #!/usr/bin/env bash
    set -euo pipefail
    BRANCH=$(git rev-parse --abbrev-ref HEAD)
    DEFAULT_BRANCH=$(git rev-parse --abbrev-ref origin/HEAD 2>/dev/null | sed 's|^origin/||' || true)
    if [ -z "${DEFAULT_BRANCH:-}" ]; then
        DEFAULT_BRANCH=$(git remote show origin 2>/dev/null | awk '/HEAD branch/ {print $NF}' || echo master)
    fi
    if [ "$BRANCH" != "$DEFAULT_BRANCH" ]; then
        echo "Error: not on default branch '$DEFAULT_BRANCH' (currently '$BRANCH')." >&2
        exit 1
    fi
    just check
    # Only restage TRACKED files (-u): `add -A` once swept a 34MB stray binary
    # into a release commit. Formatting can only ever touch tracked files.
    if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
        echo "Formatting/lint produced changes — staging + committing."
        git add -u
        git commit -m "chore: format code for release"
    fi
    # Repo hygiene: no tracked file over 1MB — a compiled binary or other
    # blob in the tree bloats every clone forever.
    BIG=$(git ls-files -z | xargs -0 -r du -b -- 2>/dev/null | awk '$1 > 1048576 {print $2}') || true
    if [ -n "$BIG" ]; then
        echo "Error: tracked files over 1MB — untrack before releasing:" >&2
        echo "$BIG" >&2
        exit 1
    fi

_release bump:
    #!/usr/bin/env bash
    set -euo pipefail
    just _release-checks
    CURRENT_TAG=$(git tag -l 'v*.*.*' --sort=-v:refname | head -1)
    CURRENT_TAG=${CURRENT_TAG:-v0.0.0}
    CURRENT_VERSION=${CURRENT_TAG#v}
    MAJOR=$(echo "$CURRENT_VERSION" | cut -d. -f1)
    MINOR=$(echo "$CURRENT_VERSION" | cut -d. -f2)
    PATCH=$(echo "$CURRENT_VERSION" | cut -d. -f3)
    case "{{bump}}" in
        major) NEW="$((MAJOR + 1)).0.0" ;;
        minor) NEW="${MAJOR}.$((MINOR + 1)).0" ;;
        patch) NEW="${MAJOR}.${MINOR}.$((PATCH + 1))" ;;
        *) echo "unknown bump kind: {{bump}}"; exit 1 ;;
    esac
    # Sync flake.nix vendorHash + version BEFORE tagging so `nix profile
    # install` never reports a stale version.
    just sync-flake "${NEW}"
    if [ -n "$(git status --porcelain flake.nix)" ]; then
        git add flake.nix
        git commit -m "chore: bump flake.nix to v${NEW}"
    fi
    git tag -a "v${NEW}" -m "v${NEW}"
    git push origin HEAD
    git push origin "v${NEW}"
    echo
    echo "Tagged v${NEW}."
    echo "Watch the release build: gh run watch || open https://github.com/stubbedev/parts-finder-mcp/actions"

release-patch: (_release "patch")
release-minor: (_release "minor")
release-major: (_release "major")
