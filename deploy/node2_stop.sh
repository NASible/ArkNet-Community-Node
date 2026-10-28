# Stop and remove all Node Agent test containers/networks/volumes
# Usage: ./deploy/node2_stop.sh
cd "$(dirname "$0")/node"
docker compose --project-name arknet-node2-dev down --volumes 2>&1
