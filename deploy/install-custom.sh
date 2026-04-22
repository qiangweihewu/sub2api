#!/bin/bash
#
# Sub2API Custom Fork - One-click Deploy Script
# Usage: curl -sSL https://raw.githubusercontent.com/qiangweihewu/sub2api/main/deploy/install-custom.sh | sudo bash
#
# This script:
#   1. Installs Docker & Docker Compose if missing
#   2. Clones the repo and builds a custom Docker image
#   3. Generates secure secrets (.env)
#   4. Starts all services (sub2api + PostgreSQL + Redis)
#

set -e

# =============================================================================
# Configuration
# =============================================================================
GITHUB_REPO="qiangweihewu/sub2api"
INSTALL_DIR="/opt/sub2api"
IMAGE_NAME="sub2api-custom"
COMPOSE_PROJECT="sub2api"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

print_info()    { echo -e "${BLUE}[INFO]${NC} $1"; }
print_success() { echo -e "${GREEN}[ OK ]${NC} $1"; }
print_warning() { echo -e "${YELLOW}[WARN]${NC} $1"; }
print_error()   { echo -e "${RED}[ERR ]${NC} $1"; }

# Generate random secret
generate_secret() { openssl rand -hex 32; }

# Check if /dev/tty is available for interactive prompts
is_interactive() { [ -e /dev/tty ] && [ -r /dev/tty ] && [ -w /dev/tty ]; }

# Validate port number
validate_port() {
    local port="$1"
    [[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ]
}

# =============================================================================
# Pre-flight Checks
# =============================================================================
check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        print_error "Please run as root (use sudo)"
        exit 1
    fi
}

check_os() {
    if [ ! -f /etc/os-release ]; then
        print_error "Unsupported OS (no /etc/os-release found)"
        exit 1
    fi
    . /etc/os-release
    print_info "Detected OS: $PRETTY_NAME"
}

# =============================================================================
# Install Docker if Missing
# =============================================================================
install_docker() {
    if command -v docker &>/dev/null; then
        print_success "Docker already installed: $(docker --version)"
        return
    fi

    print_info "Installing Docker..."
    curl -fsSL https://get.docker.com | sh
    systemctl enable docker
    systemctl start docker
    print_success "Docker installed: $(docker --version)"
}

install_docker_compose() {
    # Docker Compose V2 is bundled as a Docker plugin
    if docker compose version &>/dev/null; then
        print_success "Docker Compose already available: $(docker compose version --short)"
        return
    fi

    print_info "Installing Docker Compose plugin..."
    apt-get update -qq && apt-get install -y -qq docker-compose-plugin 2>/dev/null \
        || yum install -y docker-compose-plugin 2>/dev/null \
        || {
            # Manual install as fallback
            local compose_version
            compose_version=$(curl -s https://api.github.com/repos/docker/compose/releases/latest | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
            curl -SL "https://github.com/docker/compose/releases/download/${compose_version}/docker-compose-$(uname -s)-$(uname -m)" -o /usr/local/bin/docker-compose
            chmod +x /usr/local/bin/docker-compose
            ln -sf /usr/local/bin/docker-compose /usr/libexec/docker/cli-plugins/docker-compose 2>/dev/null || true
        }
    print_success "Docker Compose installed"
}

# =============================================================================
# Ensure Sufficient Memory (add swap if needed)
# =============================================================================
ensure_memory() {
    local total_mem_kb
    total_mem_kb=$(grep MemTotal /proc/meminfo 2>/dev/null | awk '{print $2}' || echo "0")
    local total_swap_kb
    total_swap_kb=$(grep SwapTotal /proc/meminfo 2>/dev/null | awk '{print $2}' || echo "0")
    local total_available=$(( (total_mem_kb + total_swap_kb) / 1024 ))

    print_info "System memory: $((total_mem_kb / 1024))MB RAM + $((total_swap_kb / 1024))MB swap"

    if [ "$total_available" -lt 3000 ]; then
        print_warning "Less than 3GB total memory. Docker build needs more."
        if [ "$total_swap_kb" -lt 2097152 ]; then
            print_info "Creating 2GB swap file for build..."
            if [ ! -f /swapfile ]; then
                dd if=/dev/zero of=/swapfile bs=1M count=2048 status=progress
                chmod 600 /swapfile
                mkswap /swapfile
            fi
            swapon /swapfile 2>/dev/null || true
            # Persist across reboots
            if ! grep -q '/swapfile' /etc/fstab 2>/dev/null; then
                echo '/swapfile none swap sw 0 0' >> /etc/fstab
            fi
            print_success "Swap enabled: $(swapon --show --noheadings | awk '{print $3}')"
        else
            print_info "Swap already sufficient"
        fi
    else
        print_success "Memory sufficient for build"
    fi
}

# =============================================================================
# Clone & Build
# =============================================================================
clone_repo() {
    if [ -d "$INSTALL_DIR/.git" ]; then
        print_info "Repo already exists, pulling latest..."
        cd "$INSTALL_DIR"
        git pull origin main
    else
        print_info "Cloning repo from github.com/$GITHUB_REPO ..."
        rm -rf "$INSTALL_DIR"
        git clone "https://github.com/${GITHUB_REPO}.git" "$INSTALL_DIR"
        cd "$INSTALL_DIR"
    fi
    print_success "Source code ready at $INSTALL_DIR"
}

build_image() {
    cd "$INSTALL_DIR"
    print_info "Building Docker image (this may take a few minutes)..."
    docker build -t "$IMAGE_NAME:latest" .
    print_success "Docker image built: $IMAGE_NAME:latest"
}

# =============================================================================
# Configure Environment
# =============================================================================
configure_env() {
    local env_file="$INSTALL_DIR/deploy/.env"
    local compose_dir="$INSTALL_DIR/deploy"

    # If .env already exists, ask before overwriting
    if [ -f "$env_file" ]; then
        print_warning ".env already exists at $env_file"
        if is_interactive; then
            read -p "Overwrite? Existing secrets will be lost. (y/N): " -r < /dev/tty
            echo
            if [[ ! $REPLY =~ ^[Yy]$ ]]; then
                print_info "Keeping existing .env"
                return
            fi
        else
            print_info "Keeping existing .env (non-interactive mode)"
            return
        fi
    fi

    # Collect settings interactively or use defaults
    local server_port="8080"
    local admin_email="admin@sub2api.local"
    local admin_password=""
    local tz="UTC"

    if is_interactive; then
        echo ""
        echo -e "${CYAN}=============================================="
        echo "  Server Configuration"
        echo "==============================================${NC}"
        echo ""

        read -p "Server port [8080]: " input_port < /dev/tty
        if [ -n "$input_port" ] && validate_port "$input_port"; then
            server_port="$input_port"
        fi

        read -p "Admin email [admin@sub2api.local]: " input_email < /dev/tty
        if [ -n "$input_email" ]; then
            admin_email="$input_email"
        fi

        read -p "Admin password (empty = auto-generate, shown in logs): " input_pw < /dev/tty
        if [ -n "$input_pw" ]; then
            admin_password="$input_pw"
        fi

        read -p "Timezone [UTC]: " input_tz < /dev/tty
        if [ -n "$input_tz" ]; then
            tz="$input_tz"
        fi

        echo ""
    fi

    # Generate secrets
    local pg_password jwt_secret totp_key
    pg_password=$(generate_secret)
    jwt_secret=$(generate_secret)
    totp_key=$(generate_secret)

    # Write .env
    cat > "$env_file" << EOF
# =============================================================================
# Sub2API Custom Deploy - Auto-generated $(date -u +%Y-%m-%dT%H:%M:%SZ)
# =============================================================================

# Server
BIND_HOST=0.0.0.0
SERVER_PORT=${server_port}
SERVER_MODE=release
RUN_MODE=standard
TZ=${tz}

# PostgreSQL
POSTGRES_USER=sub2api
POSTGRES_PASSWORD=${pg_password}
POSTGRES_DB=sub2api

# Redis (no password for internal network)
REDIS_PASSWORD=

# Admin
ADMIN_EMAIL=${admin_email}
ADMIN_PASSWORD=${admin_password}

# Security (auto-generated, do NOT lose these)
JWT_SECRET=${jwt_secret}
TOTP_ENCRYPTION_KEY=${totp_key}
EOF

    chmod 600 "$env_file"

    print_success "Environment configured at $env_file"
    echo ""
    echo -e "  ${YELLOW}IMPORTANT: Save these credentials securely${NC}"
    echo "  PostgreSQL password:  ${pg_password}"
    echo "  JWT secret:           ${jwt_secret:0:16}..."
    echo "  TOTP key:             ${totp_key:0:16}..."
    echo ""
}

# =============================================================================
# Create Compose Override (use custom image instead of weishaw/sub2api)
# =============================================================================
create_compose_override() {
    local override_file="$INSTALL_DIR/deploy/docker-compose.override.yml"

    cat > "$override_file" << EOF
# Auto-generated: use locally built image instead of upstream
services:
  sub2api:
    image: ${IMAGE_NAME}:latest
EOF

    print_success "Compose override created (using local image)"
}

# =============================================================================
# Start Services
# =============================================================================
start_services() {
    cd "$INSTALL_DIR/deploy"

    print_info "Starting services..."
    docker compose -p "$COMPOSE_PROJECT" up -d

    # Wait for health check
    print_info "Waiting for services to become healthy..."
    local retries=0
    while [ $retries -lt 30 ]; do
        if docker compose -p "$COMPOSE_PROJECT" ps --format json 2>/dev/null | grep -q '"Health":"healthy"'; then
            break
        fi
        sleep 2
        retries=$((retries + 1))
    done

    print_success "All services started"
}

# =============================================================================
# Upgrade
# =============================================================================
do_upgrade() {
    cd "$INSTALL_DIR"

    print_info "Pulling latest code..."
    git pull origin main

    print_info "Rebuilding Docker image..."
    ensure_memory
    docker build -t "$IMAGE_NAME:latest" .

    cd "$INSTALL_DIR/deploy"
    print_info "Restarting services..."
    docker compose -p "$COMPOSE_PROJECT" up -d

    print_success "Upgrade complete!"
}

# =============================================================================
# Rollback (swap to the previous image tag)
# =============================================================================
do_rollback() {
    if ! docker image inspect "${IMAGE_NAME}:previous" >/dev/null 2>&1; then
        print_error "No ${IMAGE_NAME}:previous image found. Cannot rollback."
        print_info "To install a specific prior version, run:"
        print_info "  VERSION=vX.Y.Z curl -sSL <install-custom-url> | sudo -E bash -s -- upgrade"
        exit 1
    fi

    local failed_tag
    failed_tag="${IMAGE_NAME}:failed-$(date +%Y%m%d-%H%M%S)"
    print_info "Archiving current ${IMAGE_NAME}:latest as $failed_tag ..."
    docker tag "${IMAGE_NAME}:latest" "$failed_tag" 2>/dev/null || true

    print_info "Swapping ${IMAGE_NAME}:previous → ${IMAGE_NAME}:latest ..."
    docker tag "${IMAGE_NAME}:previous" "${IMAGE_NAME}:latest"

    cd "$INSTALL_DIR/deploy"
    print_info "Restarting sub2api container..."
    docker compose -p "$COMPOSE_PROJECT" up -d sub2api

    print_info "Waiting for health check (max 60s)..."
    local retries=0
    while [ $retries -lt 30 ]; do
        local status
        status=$(docker inspect --format '{{.State.Health.Status}}' sub2api 2>/dev/null || echo "starting")
        if [ "$status" = "healthy" ]; then
            print_success "Rollback succeeded. Current version:"
            docker inspect --format '  {{ index .Config.Labels "sub2api.version" }}' sub2api 2>/dev/null || echo "  (no label)"
            return 0
        fi
        sleep 2
        retries=$((retries + 1))
    done

    print_error "Rollback container failed health check after 60s."
    print_error "Check logs: docker compose -p ${COMPOSE_PROJECT} logs sub2api"
    exit 1
}

# =============================================================================
# Uninstall
# =============================================================================
do_uninstall() {
    print_warning "This will stop and remove all Sub2API containers and data."
    if is_interactive; then
        read -p "Are you sure? (y/N): " -r < /dev/tty
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            print_info "Cancelled."
            exit 0
        fi
    fi

    cd "$INSTALL_DIR/deploy" 2>/dev/null && \
        docker compose -p "$COMPOSE_PROJECT" down -v 2>/dev/null || true

    docker rmi "$IMAGE_NAME:latest" 2>/dev/null || true

    if is_interactive; then
        read -p "Also remove source code and config ($INSTALL_DIR)? (y/N): " -r < /dev/tty
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            rm -rf "$INSTALL_DIR"
            print_success "All files removed"
        else
            print_info "Source code kept at $INSTALL_DIR"
        fi
    fi

    print_success "Sub2API uninstalled"
}

# =============================================================================
# Print Completion
# =============================================================================
print_completion() {
    local server_port
    server_port=$(grep '^SERVER_PORT=' "$INSTALL_DIR/deploy/.env" 2>/dev/null | cut -d= -f2 || echo "8080")

    # Try to get public IP
    local display_ip
    display_ip=$(curl -s --connect-timeout 5 --max-time 10 https://ipinfo.io/ip 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}' || echo "YOUR_SERVER_IP")

    echo ""
    echo -e "${GREEN}=============================================="
    echo "  Sub2API deployed successfully!"
    echo "==============================================${NC}"
    echo ""
    echo "  Access:  http://${display_ip}:${server_port}"
    echo ""
    echo -e "  ${CYAN}Useful Commands:${NC}"
    echo "  cd $INSTALL_DIR/deploy"
    echo ""
    echo "  View logs:      docker compose -p sub2api logs -f sub2api"
    echo "  Restart:        docker compose -p sub2api restart"
    echo "  Stop:           docker compose -p sub2api down"
    echo "  Upgrade:        curl -sSL https://raw.githubusercontent.com/${GITHUB_REPO}/main/deploy/install-custom.sh | sudo bash -s -- upgrade"
    echo ""
    echo -e "  ${YELLOW}Reverse Proxy:${NC}"
    echo "  Point your Nginx/Caddy to http://127.0.0.1:${server_port}"
    echo ""
}

# =============================================================================
# Main
# =============================================================================
main() {
    echo ""
    echo -e "${CYAN}=============================================="
    echo "  Sub2API Custom Fork - Deploy Script"
    echo "==============================================${NC}"
    echo ""

    case "${1:-}" in
        upgrade|update)
            check_root
            if [ ! -d "$INSTALL_DIR/.git" ]; then
                print_error "Sub2API not installed. Run without arguments to install first."
                exit 1
            fi
            do_upgrade
            exit 0
            ;;
        uninstall|remove)
            check_root
            do_uninstall
            exit 0
            ;;
        --help|-h)
            echo "Usage: $0 [command]"
            echo ""
            echo "Commands:"
            echo "  (none)     Install Sub2API (clone, build, start)"
            echo "  upgrade    Pull latest code, rebuild, restart"
            echo "  uninstall  Stop and remove everything"
            echo ""
            exit 0
            ;;
    esac

    # Default: Full install
    check_root
    check_os
    install_docker
    install_docker_compose
    ensure_memory
    clone_repo
    build_image
    configure_env
    create_compose_override
    start_services
    print_completion
}

main "$@"
