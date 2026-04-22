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

main() {
    print_info "release.sh skeleton (no-op for now)"
}

# Only run main if executed directly (allows sourcing for tests)
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
