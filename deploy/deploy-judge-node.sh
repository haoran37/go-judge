#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-haoran37/hnieoj-go-judge}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
IMAGE="${IMAGE:-${IMAGE_REPOSITORY}:${IMAGE_TAG}}"
CONTAINER_NAME="${CONTAINER_NAME:-hnieoj-judge-node}"
STATE_DIR="${STATE_DIR:-/var/lib/hnieoj-judge-node}"
CACHE_DIR="${CACHE_DIR:-/data/oj/judge-cache}"
WEBUI_HOST_PORT="${WEBUI_HOST_PORT:-3723}"

log() {
  printf '[%s] %s\n' "$(date '+%F %T')" "$*"
}

fail() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "缺少命令：$1"
}

check_linux() {
  local kernel_name
  kernel_name="$(uname -s 2>/dev/null || true)"
  [[ "${kernel_name}" == "Linux" ]] || fail "当前脚本仅支持 Linux 系统"
}

check_docker() {
  require_command docker
  docker info >/dev/null 2>&1 || fail "无法连接 Docker daemon，请确认 Docker 已启动且当前用户有权限访问"
}

prepare_dirs() {
  mkdir -p "${STATE_DIR}" "${CACHE_DIR}"
  chmod 700 "${STATE_DIR}" 2>/dev/null || true
  chmod 755 "${CACHE_DIR}" 2>/dev/null || true
}

preflight() {
  check_linux
  check_docker
  prepare_dirs
}

pull_image() {
  preflight
  log "正在拉取镜像：${IMAGE}"
  docker pull "${IMAGE}"
}

deploy() {
  preflight
  pull_image
  if docker ps -a --format '{{.Names}}' | grep -Fxq "${CONTAINER_NAME}"; then
    log "正在停止并删除旧容器：${CONTAINER_NAME}"
    docker rm -f "${CONTAINER_NAME}" >/dev/null
  fi
  log "正在启动 WebUI 判题机容器：${CONTAINER_NAME}"
  docker run -d \
    --name "${CONTAINER_NAME}" \
    --restart unless-stopped \
    --privileged \
    --shm-size=512m \
    -p "${WEBUI_HOST_PORT}:3723" \
    -v "${STATE_DIR}:/var/lib/hnieoj-judge-node" \
    -v "${CACHE_DIR}:/data/oj/judge-cache" \
    "${IMAGE}" >/dev/null
  log "部署完成"
  log "WebUI 地址：http://127.0.0.1:${WEBUI_HOST_PORT}"
  log "首次访问后在页面中创建管理员密码，并完成正式/临时节点初始化"
}

restart() {
  preflight
  docker restart "${CONTAINER_NAME}"
}

ps() {
  check_docker
  docker ps -a --filter "name=^/${CONTAINER_NAME}$"
}

logs() {
  check_docker
  docker logs -f --tail="${TAIL:-200}" "${CONTAINER_NAME}"
}

down() {
  check_docker
  docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
  log "容器已删除：${CONTAINER_NAME}"
}

usage() {
  cat <<EOF
用法：
  bash deploy/deploy-judge-node.sh <command>

命令：
  deploy     拉取镜像并重建 WebUI 判题机容器
  pull       只拉取镜像
  restart    重启容器
  ps         查看容器状态
  logs       查看容器日志
  down       停止并删除容器
  help       显示帮助

常用环境变量：
  IMAGE_REPOSITORY=${IMAGE_REPOSITORY}
  IMAGE_TAG=${IMAGE_TAG}
  IMAGE=${IMAGE}
  CONTAINER_NAME=${CONTAINER_NAME}
  STATE_DIR=${STATE_DIR}
  CACHE_DIR=${CACHE_DIR}
  WEBUI_HOST_PORT=${WEBUI_HOST_PORT}

端口说明：
  容器内 WebUI 固定监听 3723。
  如需修改宿主机访问端口，只修改 Docker 端口映射，例如：
  WEBUI_HOST_PORT=8080 bash deploy/deploy-judge-node.sh deploy
EOF
}

main() {
  local command="${1:-deploy}"
  case "${command}" in
    deploy) deploy ;;
    pull) pull_image ;;
    restart) restart ;;
    ps) ps ;;
    logs) logs ;;
    down) down ;;
    help|-h|--help) usage ;;
    *) usage; fail "未知命令：${command}" ;;
  esac
}

main "$@"
