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
  # 显式加固权限；chmod 失败必须暴露，绝不静默吞掉。
  chmod 700 "${STATE_DIR}"
  chmod 755 "${CACHE_DIR}"
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
  # 容器内 WebUI 显式绑定 0.0.0.0 以便端口映射；宿主机只映射到 loopback，不暴露公网。
  # 节点身份/结果队列位于 STATE_DIR（identity.json、results/），持久化且 0700。
  docker run -d \
    --name "${CONTAINER_NAME}" \
    --restart unless-stopped \
    --privileged \
    --cgroupns=host \
    --shm-size=512m \
    -e HNIEOJ_WEB_ADDR=0.0.0.0:3723 \
    -p "127.0.0.1:${WEBUI_HOST_PORT}:3723" \
    -v "${STATE_DIR}:/var/lib/hnieoj-judge-node" \
    -v "${CACHE_DIR}:/data/oj/judge-cache" \
    "${IMAGE}" >/dev/null
  log "部署完成"
  log "WebUI 地址：http://127.0.0.1:${WEBUI_HOST_PORT}"
  log "首次访问后创建管理员密码，填写一次性 Bootstrap 完成 Ed25519 入网；启动后 WSS 建立任务通道"
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
  容器内 WebUI 固定监听 3723（容器内 HNIEOJ_WEB_ADDR 显式设为 0.0.0.0 以配合端口映射）。
  宿主机默认只映射到 127.0.0.1，不对公网暴露。
  如需修改宿主机访问端口，只修改 Docker 端口映射，例如：
  WEBUI_HOST_PORT=8080 bash deploy/deploy-judge-node.sh deploy

身份与入网：
  节点身份文件与结果持久队列保存在 STATE_DIR（默认 /var/lib/hnieoj-judge-node），
  权限 0700；私钥只在此目录，绝不挂载进沙箱。首次入网用一次性 Bootstrap，
  重启复用同一 enrollmentId/key，不消耗新 Bootstrap。

cgroup 说明：
  upstream go-judge 沙箱在默认 private cgroup namespace 下可能拿不到 cgroup path
  （cgroup path empty）。脚本显式使用 --cgroupns=host，让容器使用宿主机 cgroup
  命名空间；该参数需在支持 cgroup v2 的 Linux + Docker 上验证。
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
