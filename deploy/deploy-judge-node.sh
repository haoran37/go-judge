#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-haoran37/hnieoj-go-judge}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
IMAGE="${IMAGE:-${IMAGE_REPOSITORY}:${IMAGE_TAG}}"
PROJECT_NAME="${PROJECT_NAME:-hnieoj-judge-node}"
CONFIG_DIR="${CONFIG_DIR:-/etc/hnieoj/go-judge}"
CREDENTIAL_DIR="${CREDENTIAL_DIR:-/etc/hnieoj/judge-node}"
CREDENTIAL_FILE="${CREDENTIAL_FILE:-${CREDENTIAL_DIR}/credential.json}"
CACHE_DIR="${CACHE_DIR:-/data/oj/judge-cache}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.yaml}"
COMPOSE_FILE="${COMPOSE_FILE:-${CONFIG_DIR}/compose.yaml}"

GOJUDGE_SHM_SIZE="${GOJUDGE_SHM_SIZE:-512m}"
GOJUDGE_FILE_TIMEOUT="${GOJUDGE_FILE_TIMEOUT:-30m}"
PUBLISH_GOJUDGE="${PUBLISH_GOJUDGE:-false}"
GOJUDGE_BIND_ADDR="${GOJUDGE_BIND_ADDR:-127.0.0.1}"
GOJUDGE_PUBLIC_PORT="${GOJUDGE_PUBLIC_PORT:-5050}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

log() {
  printf '[%s] %s\n' "$(date '+%F %T')" "$*"
}

warn() {
  printf '警告：%s\n' "$*" >&2
}

fail() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "缺少命令：$1"
}

check_ubuntu() {
  local os_id=""
  local os_pretty=""
  [[ -r /etc/os-release ]] || fail "当前只兼容 Ubuntu，未找到 /etc/os-release"
  # 读取系统发行版标识，用于限制当前脚本只在 Ubuntu 上运行。
  # shellcheck disable=SC1091
  . /etc/os-release
  os_id="${ID:-}"
  os_pretty="${PRETTY_NAME:-未知 Linux}"
  [[ "${os_id}" == "ubuntu" ]] || fail "当前只兼容 Ubuntu，当前系统为：${os_pretty}"
}

check_default_path_permissions() {
  local path
  if [[ "${EUID}" -eq 0 ]]; then
    return
  fi
  for path in "${CONFIG_DIR}" "${CREDENTIAL_DIR}" "${CACHE_DIR}"; do
    case "${path}" in
      /etc/*|/data/*|/var/*)
        fail "默认部署目录 ${path} 需要 root 权限；请使用 sudo 执行，或通过 CONFIG_DIR/CREDENTIAL_DIR/CACHE_DIR 指定其他目录"
        ;;
    esac
  done
}

check_docker() {
  require_command docker
  docker compose version >/dev/null 2>&1 || fail "Docker Compose v2 插件不可用，请先安装 Docker Compose v2 plugin"
  docker info >/dev/null 2>&1 || fail "无法连接 Docker daemon，请确认 Docker 已启动，且当前用户有权限访问 Docker"
}

docker_compose() {
  docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" "$@"
}

is_true() {
  case "${1:-}" in
    1|true|TRUE|yes|YES|y|Y|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

read_line() {
  local variable_name="$1"
  IFS= read -r "${variable_name}"
}

prompt() {
  local label="$1"
  local default_value="$2"
  local value
  printf '%s [%s]: ' "${label}" "${default_value}" >&2
  read_line value
  if [[ -z "${value}" ]]; then
    printf '%s' "${default_value}"
  else
    printf '%s' "${value}"
  fi
}

prompt_secret_required() {
  local label="$1"
  local value
  while true; do
    printf '%s: ' "${label}" >&2
    read -r -s value
    printf '\n' >&2
    if [[ -n "${value}" ]]; then
      printf '%s' "${value}"
      return
    fi
    warn "该项不能为空"
  done
}

prompt_positive_int() {
  local label="$1"
  local default_value="$2"
  local value
  while true; do
    value="$(prompt "${label}" "${default_value}")"
    if [[ "${value}" =~ ^[1-9][0-9]*$ ]]; then
      printf '%s' "${value}"
      return
    fi
    warn "请输入正整数"
  done
}

confirm() {
  local question="$1"
  local answer
  printf '%s [y/N]: ' "${question}" >&2
  read_line answer
  case "${answer}" in
    y|Y|yes|YES) return 0 ;;
    *) return 1 ;;
  esac
}

yaml_quote() {
  local value="${1:-}"
  [[ "${value}" != *$'\n'* ]] || fail "YAML 字段不能包含换行"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "${value}"
}

array_contains() {
  local needle="$1"
  local item
  shift
  for item in "$@"; do
    [[ "${item}" == "${needle}" ]] && return 0
  done
  return 1
}

normalize_modes_csv() {
  local raw="$1"
  local part
  local parts=()
  local out=()
  raw="${raw// /}"
  [[ -n "${raw}" ]] || raw="default"
  IFS=',' read -r -a parts <<< "${raw}"
  for part in "${parts[@]}"; do
    case "${part}" in
      default|spj|interactive) ;;
      "") continue ;;
      *) fail "不支持的判题模式：${part}" ;;
    esac
    if ! array_contains "${part}" "${out[@]}"; then
      out+=("${part}")
    fi
  done
  [[ "${#out[@]}" -gt 0 ]] || out=("default")
  (IFS=','; printf '%s' "${out[*]}")
}

write_modes_yaml() {
  local modes_csv="$1"
  local mode
  local modes=()
  IFS=',' read -r -a modes <<< "${modes_csv}"
  for mode in "${modes[@]}"; do
    printf '    - %s\n' "${mode}"
  done
}

prepare_dirs() {
  mkdir -p "${CONFIG_DIR}" "${CREDENTIAL_DIR}" "${CACHE_DIR}"
  chmod 700 "${CONFIG_DIR}" "${CREDENTIAL_DIR}" 2>/dev/null || true
  chmod 755 "${CACHE_DIR}" 2>/dev/null || true
}

check_source_tree() {
  [[ -f "${SOURCE_DIR}/Dockerfile.hnieoj" ]] || fail "在 ${SOURCE_DIR} 下未找到 Dockerfile.hnieoj；远程下载脚本只支持拉取镜像部署，本地构建请在源码仓库中执行"
}

preflight() {
  check_ubuntu
  check_default_path_permissions
  check_docker
  prepare_dirs
}

write_credential_file() {
  local content="$1"
  local tmp_file
  tmp_file="$(mktemp "${CREDENTIAL_DIR}/credential.json.tmp.XXXXXX")"
  printf '%s\n' "${content}" > "${tmp_file}"
  chmod 600 "${tmp_file}" 2>/dev/null || true
  mv "${tmp_file}" "${CREDENTIAL_FILE}"
  log "已写入逐节点运行凭证：${CREDENTIAL_FILE}"
}

write_config_file() {
  local node_name="$1"
  local node_type="$2"
  local max_concurrency="$3"
  local supported_modes="$4"
  local backend_url="$5"
  local auth_code="$6"

  local tmp_file
  tmp_file="$(mktemp "${CONFIG_DIR}/config.yaml.tmp.XXXXXX")"

  {
    cat <<EOF
# HnieOJ 判题节点运行配置。
# 由 deploy/deploy-judge-node.sh 生成。节点运行期只访问后端 HTTPS 网关和本地沙箱，
# 不依赖 RabbitMQ/Nacos/Redis。真实授权码与运行凭证不要提交到仓库。

node:
  # 节点名称，建议全局唯一。
  name: $(yaml_quote "${node_name}")
  # 节点类型：formal 为正式长期节点，temp 为临时节点。
  type: $(yaml_quote "${node_type}")
  # 最大并发判题任务数。后端以核准额度为准。
  maxConcurrency: ${max_concurrency}
  # 本节点支持的判题模式。确认后端和题目协议闭环后再开启 spj/interactive。
  supportedJudgeModes:
EOF
    write_modes_yaml "${supported_modes}"
    cat <<EOF

hnieoj:
  # HnieOJ 后端网关地址。远程必须 HTTPS；仅回环地址允许明文 HTTP 供本地开发。
  baseUrl: $(yaml_quote "${backend_url}")
  # 后端接口 HTTP 超时。
  requestTimeout: "30s"
  credential:
    # 逐节点运行凭证文件。formal 由运维交付；temp 首次注册后自动写入；续期后原子替换（0600）。
    tokenFile: $(yaml_quote "${CREDENTIAL_FILE}")
    # 也可内联逐节点 Bearer JWT；留空只使用凭证文件。不要提交真实值。
    token: ""
    # temp 首次接入授权码，仅首次注册使用；formal 必须留空。
    authCode: $(yaml_quote "${auth_code}")
  renew:
    # 依后端 JWT exp 提前续期的安全余量。
    safetyMargin: "30s"
    # 续期网络失败重试退避。
    retryBackoff: "10s"

testdata:
  # 测试数据缓存目录。
  cacheRoot: "/data/oj/judge-cache"
  # 缓存最大字节数。0 表示不按容量清理。
  maxCacheBytes: 21474836480
  # 多久未使用后可清理。0 表示不按时间清理。
  maxUnusedDuration: "72h"
  # 缓存清理任务执行间隔。
  cleanupInterval: "1h"
  # 心跳缓存/磁盘统计采样间隔。
  statsInterval: "5m"

gojudge:
  # go-judge sandbox 服务地址（本地/内网 HTTP）。
  endpoint: "http://go-judge-sandbox:5050"
  authToken: ""

reporter:
  # http 上报后端；log 仅用于本地调试/fixture。
  mode: "http"
  endpoint: "/judge/submissions/{submissionId}/events"
  # 仅对网络失败/5xx 有限重试；HTTP200 但 Result.code != 200 立即失败。
  maxRetries: 3
  retryBackoff: "2s"

heartbeat:
  # 生产环境建议开启心跳，间隔不要设置为 1 秒级别。
  enabled: true
  endpoint: "/judge/nodes/heartbeat"
  interval: "30s"

worker:
  # 空队列退避（有上限 + 抖动）。
  emptyMinBackoff: "200ms"
  emptyMaxBackoff: "5s"
  # SIGTERM 后停止领取并在该窗口内排空在途任务，超时取消。
  drainTimeout: "5m"
EOF
  } > "${tmp_file}"

  chmod 600 "${tmp_file}" 2>/dev/null || true
  mv "${tmp_file}" "${CONFIG_FILE}"
  log "已写入配置文件：${CONFIG_FILE}"
}

init_config() {
  check_ubuntu
  check_default_path_permissions
  prepare_dirs
  if [[ -f "${CONFIG_FILE}" ]] && ! is_true "${FORCE:-false}"; then
    confirm "是否覆盖已有配置文件 ${CONFIG_FILE}？" || fail "配置未修改"
  fi

  local node_type_choice
  local node_type
  while true; do
    node_type_choice="$(prompt "节点类型：1=formal，2=temp" "1")"
    case "${node_type_choice}" in
      1|formal) node_type="formal"; break ;;
      2|temp) node_type="temp"; break ;;
      *) warn "请输入 1、2、formal 或 temp" ;;
    esac
  done

  local node_name
  local max_concurrency
  local supported_modes
  local backend_url
  local auth_code=""

  node_name="$(prompt "节点名称" "judge-node-01")"
  max_concurrency="$(prompt_positive_int "最大并发判题任务数" "2")"
  supported_modes="$(normalize_modes_csv "$(prompt "支持的判题模式" "default")")"
  backend_url="$(prompt "HnieOJ 后端网关地址（远程必须 https）" "https://oj.example.com")"

  if [[ "${node_type}" == "formal" ]]; then
    if [[ -f "${CREDENTIAL_FILE}" ]]; then
      chmod 600 "${CREDENTIAL_FILE}" 2>/dev/null || true
      log "检测到已有正式节点凭证：${CREDENTIAL_FILE}"
    else
      warn "未找到正式节点凭证：${CREDENTIAL_FILE}"
      warn "请管理员调用 POST /api/admin/judge/nodes/formal-tokens 签发，并把返回 data JSON 写入该文件。"
      local bearer_token
      bearer_token="$(prompt "可直接粘贴 Bearer JWT（留空稍后手工写入凭证文件）" "")"
      if [[ -n "${bearer_token}" ]]; then
        write_credential_file "{\"tokenType\":\"Bearer\",\"token\":$(yaml_quote "${bearer_token}")}"
      fi
    fi
  else
    while true; do
      auth_code="$(prompt_secret_required "临时节点授权码")"
      if [[ -n "${auth_code}" ]]; then
        break
      fi
      warn "临时节点首次接入需要授权码"
    done
    log "temp 节点将在首次启动时用授权码注册，并把凭证原子写入 ${CREDENTIAL_FILE}"
  fi

  write_config_file "${node_name}" "${node_type}" "${max_concurrency}" "${supported_modes}" "${backend_url}" "${auth_code}"
}

render_compose() {
  check_ubuntu
  check_default_path_permissions
  prepare_dirs
  [[ -f "${CONFIG_FILE}" ]] || fail "缺少配置文件：${CONFIG_FILE}；请先执行 '$0 init'"

  local tmp_file
  tmp_file="$(mktemp "${CONFIG_DIR}/compose.yaml.tmp.XXXXXX")"

  {
    cat <<EOF
services:
  go-judge-sandbox:
    image: ${IMAGE}
    restart: unless-stopped
    privileged: true
    shm_size: ${GOJUDGE_SHM_SIZE}
    command:
      - /usr/local/bin/go-judge
      - -http-addr=:5050
      - -mount-conf=/opt/go-judge/mount.yaml
      - -file-timeout=${GOJUDGE_FILE_TIMEOUT}
EOF
    if is_true "${PUBLISH_GOJUDGE}"; then
      cat <<EOF
    ports:
      - "${GOJUDGE_BIND_ADDR}:${GOJUDGE_PUBLIC_PORT}:5050"
EOF
    fi
    cat <<EOF
    networks:
      - hnieoj-judge
    logging:
      driver: json-file
      options:
        max-size: "50m"
        max-file: "3"

  hnieoj-judge-node:
    image: ${IMAGE}
    restart: unless-stopped
    depends_on:
      - go-judge-sandbox
    command:
      - /usr/local/bin/hnieoj-judge-node
      - -config
      - /etc/hnieoj/go-judge/config.yaml
    volumes:
      - "${CONFIG_FILE}:/etc/hnieoj/go-judge/config.yaml:ro"
      # 凭证目录必须可写：续期成功后节点会原子替换 credential.json。
      - "${CREDENTIAL_DIR}:/etc/hnieoj/judge-node"
      - "${CACHE_DIR}:/data/oj/judge-cache"
    networks:
      - hnieoj-judge
    logging:
      driver: json-file
      options:
        max-size: "50m"
        max-file: "3"

networks:
  hnieoj-judge:
    driver: bridge
EOF
  } > "${tmp_file}"

  chmod 600 "${tmp_file}" 2>/dev/null || true
  mv "${tmp_file}" "${COMPOSE_FILE}"
  log "已写入 Compose 文件：${COMPOSE_FILE}"
}

doctor() {
  preflight
  [[ -f "${CONFIG_FILE}" ]] || fail "缺少配置文件：${CONFIG_FILE}"
  [[ -f "${COMPOSE_FILE}" ]] || fail "缺少 Compose 文件：${COMPOSE_FILE}；请执行 '$0 render'"
  if grep -Eq 'type:[[:space:]]*"?formal"?' "${CONFIG_FILE}" && [[ ! -f "${CREDENTIAL_FILE}" ]]; then
    if ! grep -Eq 'token:[[:space:]]*"?[^"[:space:]]' "${CONFIG_FILE}"; then
      fail "formal 节点缺少运行凭证：请将管理员签发的凭证 JSON 写入 ${CREDENTIAL_FILE}，或在配置中填写 credential.token"
    fi
  fi
  docker_compose config >/dev/null
  log "预检查通过"
}

build_image() {
  preflight
  check_source_tree
  log "正在构建镜像：${IMAGE}"
  docker build -f "${SOURCE_DIR}/Dockerfile.hnieoj" -t "${IMAGE}" "${SOURCE_DIR}"
}

pull_image() {
  preflight
  log "正在拉取镜像：${IMAGE}"
  docker pull "${IMAGE}"
}

up() {
  render_compose
  doctor
  docker_compose up -d
  docker_compose ps
}

deploy() {
  preflight
  if [[ ! -f "${CONFIG_FILE}" ]]; then
    log "未找到配置文件，开始交互式初始化"
    init_config
  fi
  render_compose
  pull_image
  doctor
  docker_compose up -d --force-recreate --remove-orphans
  docker_compose ps
  log "部署完成"
}

usage() {
  cat <<EOF
用法：
  bash deploy/deploy-judge-node.sh <command>

命令：
  deploy        配置缺失时先初始化，然后渲染 Compose、拉取镜像并重建服务。
  init          交互式写入 ${CONFIG_FILE}（formal 需要凭证文件或 token，temp 需要授权码）。
  render        按当前环境变量和配置路径渲染 ${COMPOSE_FILE}。
  doctor        检查 Docker、配置文件、Compose 文件和 formal 节点运行凭证。
  pull          从 Docker Hub 拉取 ${IMAGE}。
  build         在源码仓库中基于 Dockerfile.hnieoj 构建 ${IMAGE}。
  up            渲染 Compose 并启动服务。
  restart       重启服务。
  ps            查看服务状态。
  logs          跟随日志；可在命令后追加服务名。
  down          停止并删除服务。
  help          显示帮助。

节点不再依赖 RabbitMQ/Nacos；运行凭证由后端逐节点签发，重启使用 ${CREDENTIAL_FILE}。

常用环境变量：
  IMAGE_REPOSITORY=${IMAGE_REPOSITORY}
  IMAGE_TAG=${IMAGE_TAG}
  IMAGE=${IMAGE}
  PROJECT_NAME=${PROJECT_NAME}
  CONFIG_DIR=${CONFIG_DIR}
  CREDENTIAL_DIR=${CREDENTIAL_DIR}
  CREDENTIAL_FILE=${CREDENTIAL_FILE}
  CACHE_DIR=${CACHE_DIR}
  GOJUDGE_FILE_TIMEOUT=${GOJUDGE_FILE_TIMEOUT}
  PUBLISH_GOJUDGE=${PUBLISH_GOJUDGE}
  GOJUDGE_BIND_ADDR=${GOJUDGE_BIND_ADDR}
  GOJUDGE_PUBLIC_PORT=${GOJUDGE_PUBLIC_PORT}

示例：
  bash deploy/deploy-judge-node.sh init
  bash deploy/deploy-judge-node.sh deploy
  IMAGE_TAG=sha-abcdef0 bash deploy/deploy-judge-node.sh deploy
  PUBLISH_GOJUDGE=true bash deploy/deploy-judge-node.sh up
  bash deploy/deploy-judge-node.sh logs hnieoj-judge-node
EOF
}

main() {
  local command="${1:-deploy}"
  shift || true
  case "${command}" in
    deploy) deploy ;;
    init|configure) init_config ;;
    render) render_compose ;;
    doctor|check) doctor ;;
    pull) pull_image ;;
    build) build_image ;;
    up) up ;;
    restart) render_compose; doctor; docker_compose restart "$@" ;;
    ps) docker_compose ps "$@" ;;
    logs) docker_compose logs -f --tail="${TAIL:-200}" "$@" ;;
    down) docker_compose down "$@" ;;
    help|-h|--help) usage ;;
    *) usage; fail "未知命令：${command}" ;;
  esac
}

main "$@"
