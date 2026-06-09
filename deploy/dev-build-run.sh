#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

REMOTE="${REMOTE:-origin}"
BRANCH="${BRANCH:-develop}"
REPO_URL="${REPO_URL:-https://github.com/haoran37/go-judge.git}"
WORK_DIR="${WORK_DIR:-/opt/hnieoj-go-judge-dev}"
IMAGE="${IMAGE:-haoran37/hnieoj-go-judge:dev-local}"
CONTAINER_NAME="${CONTAINER_NAME:-hnieoj-judge-node-dev}"
STATE_DIR="${STATE_DIR:-/tmp/hnieoj-judge-node-dev/state}"
CACHE_DIR="${CACHE_DIR:-/tmp/hnieoj-judge-node-dev/cache}"
WEBUI_HOST_PORT="${WEBUI_HOST_PORT:-3723}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if git -C "${SCRIPT_DIR}/.." rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
else
  REPO_DIR="${WORK_DIR}"
fi

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
  [[ "$(uname -s 2>/dev/null || true)" == "Linux" ]] || fail "当前脚本仅支持 Linux"
}

check_docker() {
  require_command docker
  docker info >/dev/null 2>&1 || fail "无法连接 Docker daemon"
}

update_develop() {
  require_command git
  if [[ ! -d "${REPO_DIR}/.git" ]]; then
    log "本地仓库不存在，正在克隆 ${REPO_URL} 到 ${REPO_DIR}"
    mkdir -p "$(dirname "${REPO_DIR}")"
    git clone --branch "${BRANCH}" "${REPO_URL}" "${REPO_DIR}"
    return
  fi
  log "正在尝试更新 ${REMOTE}/${BRANCH}；如有本地修改，将尽量自动保留"
  if ! git -C "${REPO_DIR}" fetch "${REMOTE}" "${BRANCH}"; then
    log "警告：fetch 失败，将继续使用当前工作区构建"
    return
  fi
  if ! git -C "${REPO_DIR}" checkout "${BRANCH}"; then
    log "警告：切换到 ${BRANCH} 失败，将继续使用当前分支构建"
    return
  fi
  if ! git -C "${REPO_DIR}" pull --ff-only --autostash "${REMOTE}" "${BRANCH}"; then
    log "警告：拉取最新 ${BRANCH} 失败，将保留本地修改并继续构建当前工作区"
  fi
}

build_image() {
  check_linux
  check_docker
  update_develop
  log "正在构建开发镜像：${IMAGE}"
  docker build -f "${REPO_DIR}/Dockerfile.hnieoj" -t "${IMAGE}" "${REPO_DIR}"
}

run_container() {
  check_linux
  check_docker
  mkdir -p "${STATE_DIR}" "${CACHE_DIR}"
  if docker ps -a --format '{{.Names}}' | grep -Fxq "${CONTAINER_NAME}"; then
    log "正在删除旧开发容器：${CONTAINER_NAME}"
    docker rm -f "${CONTAINER_NAME}" >/dev/null
  fi
  log "正在启动开发容器：${CONTAINER_NAME}"
  docker run -d \
    --name "${CONTAINER_NAME}" \
    --restart unless-stopped \
    --privileged \
    --shm-size=512m \
    -p "${WEBUI_HOST_PORT}:3723" \
    -v "${STATE_DIR}:/var/lib/hnieoj-judge-node" \
    -v "${CACHE_DIR}:/data/oj/judge-cache" \
    "${IMAGE}" >/dev/null
  log "开发容器已启动，WebUI：http://127.0.0.1:${WEBUI_HOST_PORT}"
}

deploy() {
  build_image
  run_container
}

logs() {
  check_docker
  docker logs -f --tail="${TAIL:-200}" "${CONTAINER_NAME}"
}

down() {
  check_docker
  docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
  log "开发容器已删除：${CONTAINER_NAME}"
}

usage() {
  cat <<EOF
用法：
  bash deploy/dev-build-run.sh <command>

命令：
  deploy   更新 ${BRANCH}、构建开发镜像并重建开发容器
  build    只更新 ${BRANCH} 并构建开发镜像
  run      使用当前开发镜像重建开发容器
  logs     查看开发容器日志
  down     删除开发容器
  help     显示帮助

常用环境变量：
  REMOTE=${REMOTE}
  BRANCH=${BRANCH}
  REPO_URL=${REPO_URL}
  WORK_DIR=${WORK_DIR}
  IMAGE=${IMAGE}
  CONTAINER_NAME=${CONTAINER_NAME}
  STATE_DIR=${STATE_DIR}
  CACHE_DIR=${CACHE_DIR}
  WEBUI_HOST_PORT=${WEBUI_HOST_PORT}

示例：
  bash deploy/dev-build-run.sh deploy
  WEBUI_HOST_PORT=8080 bash deploy/dev-build-run.sh deploy
EOF
}

main() {
  local command="${1:-deploy}"
  case "${command}" in
    deploy) deploy ;;
    build) build_image ;;
    run) run_container ;;
    logs) logs ;;
    down) down ;;
    help|-h|--help) usage ;;
    *) usage; fail "未知命令：${command}" ;;
  esac
}

main "$@"
