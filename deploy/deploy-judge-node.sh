#!/usr/bin/env bash
set -euo pipefail

IMAGE="${IMAGE:-hnieoj/go-judge:dev}"
PROJECT_NAME="${PROJECT_NAME:-hnieoj-judge-node}"
DOCKER_NETWORK="${DOCKER_NETWORK:-hnieoj-judge-net}"
RABBITMQ_CONTAINER="${RABBITMQ_CONTAINER:-rabbitmq}"
CONFIG_DIR="${CONFIG_DIR:-/etc/hnieoj/go-judge}"
SECURITY_DIR="${SECURITY_DIR:-/etc/hnieoj/judge-security}"
CACHE_DIR="${CACHE_DIR:-/data/oj/judge-cache}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.yaml}"
COMPOSE_FILE="${COMPOSE_FILE:-${CONFIG_DIR}/docker-compose.yml}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

log() {
  printf '[%s] %s\n' "$(date '+%F %T')" "$*"
}

fail() {
  printf 'Error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "缺少命令：$1"
}

compose() {
  docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" "$@"
}

prepare_environment() {
  require_command docker
  docker compose version >/dev/null
  mkdir -p "${CONFIG_DIR}" "${SECURITY_DIR}" "${CACHE_DIR}"
  chmod 700 "${CONFIG_DIR}" "${SECURITY_DIR}" || true
  chmod 755 "${CACHE_DIR}" || true
}

prepare_network() {
  if [[ "${DOCKER_NETWORK}" == "private" ]]; then
    log "使用 compose 私有网络"
    return
  fi
  if ! docker network inspect "${DOCKER_NETWORK}" >/dev/null 2>&1; then
    log "创建 Docker 网络：${DOCKER_NETWORK}"
    docker network create "${DOCKER_NETWORK}" >/dev/null
  fi
  if docker ps --format '{{.Names}}' | grep -qx "${RABBITMQ_CONTAINER}"; then
    docker network connect "${DOCKER_NETWORK}" "${RABBITMQ_CONTAINER}" >/dev/null 2>&1 || true
    log "已确保 RabbitMQ 容器接入网络：${RABBITMQ_CONTAINER} -> ${DOCKER_NETWORK}"
  else
    log "未发现 RabbitMQ 容器 ${RABBITMQ_CONTAINER}，如使用外部 RabbitMQ 请在初始化时填写外部地址。"
  fi
}

build_image() {
  log "构建镜像：${IMAGE}"
  docker build -f "${SOURCE_DIR}/Dockerfile.hnieoj" -t "${IMAGE}" "${SOURCE_DIR}"
}

run_cli() {
  local network_args=()
  if [[ "${DOCKER_NETWORK}" != "private" ]]; then
    network_args=(--network "${DOCKER_NETWORK}")
  fi
  docker run --rm -it \
    "${network_args[@]}" \
    -v "${CONFIG_DIR}:/etc/hnieoj/go-judge" \
    -v "${SECURITY_DIR}:/etc/hnieoj/judge-security" \
    -v "${CACHE_DIR}:/data/oj/judge-cache" \
    "${IMAGE}" \
    hnieoj-judge-node "$@"
}

init_config() {
  log "进入交互式初始化。Docker network 建议填写：${DOCKER_NETWORK}"
  run_cli init -config /etc/hnieoj/go-judge/config.yaml -compose /etc/hnieoj/go-judge/docker-compose.yml
}

exchange_temp_token_prompt() {
  local answer
  printf '是否需要立即兑换临时节点授权码？[y/N]: '
  read -r answer
  case "${answer}" in
    y|Y|yes|YES)
      run_cli auth-exchange -config /etc/hnieoj/go-judge/config.yaml
      ;;
    *)
      log "跳过临时授权码兑换。正式节点或已存在有效 temp-token.json 时可以跳过。"
      ;;
  esac
}

start_services() {
  [[ -f "${COMPOSE_FILE}" ]] || fail "缺少 compose 文件：${COMPOSE_FILE}，请先执行 init。"
  log "启动判题节点 compose：${COMPOSE_FILE}"
  compose up -d
  compose ps
}

deploy() {
  prepare_environment
  prepare_network
  build_image
  init_config
  exchange_temp_token_prompt
  start_services
}

usage() {
  cat <<EOF
用法：
  bash deploy/deploy-judge-node.sh [命令]

命令：
  deploy          构建镜像、交互式初始化、可选兑换临时 Token 并启动，默认命令
  init            只执行交互式初始化
  auth-exchange   只兑换临时节点授权码
  up              启动判题节点容器
  ps              查看容器状态
  logs            查看容器日志
  down            停止并移除容器
  doctor          执行基础连通性检查
  build           只构建镜像
  help            显示帮助

环境变量：
  IMAGE=${IMAGE}
  DOCKER_NETWORK=${DOCKER_NETWORK}
  RABBITMQ_CONTAINER=${RABBITMQ_CONTAINER}
  CONFIG_DIR=${CONFIG_DIR}
  SECURITY_DIR=${SECURITY_DIR}
  CACHE_DIR=${CACHE_DIR}
EOF
}

main() {
  local command="${1:-deploy}"
  case "${command}" in
    deploy) deploy ;;
    init) prepare_environment; prepare_network; build_image; init_config ;;
    auth-exchange) prepare_environment; prepare_network; run_cli auth-exchange -config /etc/hnieoj/go-judge/config.yaml ;;
    up) prepare_environment; start_services ;;
    ps) prepare_environment; compose ps ;;
    logs) prepare_environment; compose logs -f --tail=200 ;;
    down) prepare_environment; compose down ;;
    doctor) prepare_environment; prepare_network; run_cli doctor -config /etc/hnieoj/go-judge/config.yaml ;;
    build) prepare_environment; build_image ;;
    help|-h|--help) usage ;;
    *) usage; fail "未知命令：${command}" ;;
  esac
}

main "$@"
