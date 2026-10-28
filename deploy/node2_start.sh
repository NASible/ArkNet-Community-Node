#!/usr/bin/env bash
# Spin up a second Node Agent for testing
# Usage: ./deploy/node2_start.sh
#
# The second node:
# - Uses its own PostgreSQL container (different container and volume names)
# - Connects to the SAME running Core service (host.docker.internal:8085)
#   so you can approve both nodes in the same admin panel
# - Exposes port 8087 for the agent API and 5175 for the UI
# - Creates isolated Docker networks/volumes (arknet-node2-dev_)

set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "$0")" && pwd)"
NODE_DIR="$DEPLOY_DIR/node"

# Unique project prefix for Docker isolation
PROJECT="arknet-node2-dev"

echo "=== Second Node Agent ==="
echo "Agent API: http://localhost:8087"
echo "Node UI:   http://localhost:5175"
echo "Connects to Core: http://host.docker.internal:8085"
echo ""

# Start services with the project name so Docker isolates them
# from the first node's containers/networks/volumes
cd "$NODE_DIR"
docker compose -p "$PROJECT" up --build -d

sleep 10

echo "=== Service Status ==="
docker ps --filter "name=${PROJECT}" --format "table {{.Names}}\t{{.Status}}"
