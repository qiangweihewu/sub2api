#!/bin/bash
#
# scripts/release.sh
# Build sub2api binary locally (Mac) and publish as a GitHub Release.
#
# Usage:
#   ./scripts/release.sh v1.2.3       # explicit version
#   ./scripts/release.sh              # read backend/cmd/server/VERSION
#
# Requirements: go >= 1.26, pnpm, gh (GitHub CLI logged in), tar, shasum
#

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

print_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
print_success() { echo -e "${GREEN}[ OK ]${NC} $*"; }
print_warning() { echo -e "${YELLOW}[WARN]${NC} $*"; }
print_error()   { echo -e "${RED}[ERR ]${NC} $*" >&2; }

# Locate repo root (directory containing backend/, frontend/, scripts/)
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# -----------------------------------------------------------------------------
# Version parsing
# -----------------------------------------------------------------------------
resolve_version() {
    local arg="${1:-}"
    if [ -n "$arg" ]; then
        echo "$arg"
        return
    fi
    local version_file="$REPO_ROOT/backend/cmd/server/VERSION"
    if [ ! -f "$version_file" ]; then
        print_error "No version given and $version_file does not exist"
        exit 1
    fi
    echo "v$(tr -d '\r\n' < "$version_file")"
}

validate_version() {
    local v="$1"
    if ! [[ "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        print_error "Invalid version format: '$v' (expected vX.Y.Z)"
        exit 1
    fi
}

check_tag_unused() {
    local v="$1"
    if gh release view "$v" >/dev/null 2>&1; then
        print_error "Release $v already exists on GitHub. Pick a different version."
        exit 1
    fi
}

main() {
    local version
    version=$(resolve_version "${1:-}")
    validate_version "$version"
    print_info "Resolved version: $version"
    print_warning "check_tag_unused skipped (no gh calls yet in this step)"
}

# Only run main if executed directly (allows sourcing for tests)
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
