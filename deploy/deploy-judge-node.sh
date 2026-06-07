#!/usr/bin/env bash
set -euo pipefail

IMAGE="${IMAGE:-hnieoj/go-judge:dev}"
PROJECT_NAME="${PROJECT_NAME:-hnieoj-judge-node}"
CONFIG_DIR="${CONFIG_DIR:-/etc/hnieoj/go-judge}"
SECURITY_DIR="${SECURITY_DIR:-/etc/hnieoj/judge-security}"
CACHE_DIR="${CACHE_DIR:-/data/oj/judge-cache}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.yaml}"
COMPOSE_FILE="${COMPOSE_FILE:-${CONFIG_DIR}/docker-compose.yml}"
PRIVATE_KEY_FILE="${PRIVATE_KEY_FILE:-${SECURITY_DIR}/judge_formal_private.pem}"
GOJUDGE_PUBLIC_PORT="${GOJUDGE_PUBLIC_PORT:-5050}"
GOJUDGE_SHM_SIZE="${GOJUDGE_SHM_SIZE:-512m}"

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

yaml_quote() {
  local value="${1:-}"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "${value}"
}

read_line() {
  local variable_name="$1"
  if [[ -t 0 ]]; then
    IFS= read -r -e "${variable_name}"
  else
    IFS= read -r "${variable_name}"
  fi
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

prompt_required() {
  local label="$1"
  local value
  while true; do
    printf '%s: ' "${label}" >&2
    read_line value
    if [[ -n "${value}" ]]; then
      printf '%s' "${value}"
      return
    fi
    printf '该配置不能为空。\n' >&2
  done
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
    printf '该配置不能为空。\n' >&2
  done
}

prompt_int() {
  local label="$1"
  local default_value="$2"
  local value
  while true; do
    value="$(prompt "${label}" "${default_value}")"
    if [[ "${value}" =~ ^[1-9][0-9]*$ ]]; then
      printf '%s' "${value}"
      return
    fi
    printf '请输入正整数。\n' >&2
  done
}

prepare_environment() {
  require_command docker
  docker compose version >/dev/null
  mkdir -p "${CONFIG_DIR}" "${SECURITY_DIR}" "${CACHE_DIR}"
  chmod 700 "${CONFIG_DIR}" "${SECURITY_DIR}" || true
  chmod 755 "${CACHE_DIR}" || true
}

build_image() {
  log "构建镜像：${IMAGE}"
  docker build -f "${SOURCE_DIR}/Dockerfile.hnieoj" -t "${IMAGE}" "${SOURCE_DIR}"
}

write_common_config() {
  local node_name="$1"
  local node_type="$2"
  local max_concurrency="$3"
  local backend_url="$4"
  local rabbit_host="$5"
  local rabbit_port="$6"
  local rabbit_username="$7"
  local rabbit_password="$8"
  local rabbit_vhost="$9"
  local auth_code="${10}"
  local nacos_server="${11}"
  local nacos_namespace="${12}"
  local remote_enabled="${13}"
  local formal_token_group="${14}"
  local formal_token_data_id="${15}"

  cat > "${CONFIG_FILE}" <<EOF
node:
  name: $(yaml_quote "${node_name}")
  type: $(yaml_quote "${node_type}")
  maxConcurrency: ${max_concurrency}

remoteConfig:
  enabled: ${remote_enabled}
  nacos:
    serverAddr: $(yaml_quote "${nacos_server}")
    namespace: $(yaml_quote "${nacos_namespace}")
    group: "HNIEOJ_JUDGE_GROUP"
    dataId: "hnieoj-judge-node.yaml"

hnieoj:
  baseUrl: $(yaml_quote "${backend_url}")
  requestTimeout: "30s"
  formalToken:
    privateKeyPath: $(yaml_quote "${PRIVATE_KEY_FILE}")
    cipherAlgorithm: "RSA/ECB/OAEPWithSHA-256AndMGF1Padding"
    refreshInterval: "30s"
    nacos:
      serverAddr: $(yaml_quote "${nacos_server}")
      namespace: $(yaml_quote "${nacos_namespace}")
      group: $(yaml_quote "${formal_token_group}")
      dataId: $(yaml_quote "${formal_token_data_id}")
  tempToken:
    authCode: $(yaml_quote "${auth_code}")

rabbitmq:
  host: $(yaml_quote "${rabbit_host}")
  port: ${rabbit_port}
  username: $(yaml_quote "${rabbit_username}")
  password: $(yaml_quote "${rabbit_password}")
  virtualHost: $(yaml_quote "${rabbit_vhost}")
  exchange: "hnieoj.judge.exchange"
  queue: "hnieoj.judge.task"
  routingKey: "judge.submission.created"
  deadLetterExchange: "hnieoj.judge.dlx"
  deadLetterQueue: "hnieoj.judge.task.dlq"
  deadLetterRoutingKey: "judge.submission.created.dlq"
  prefetch: ${max_concurrency}
  maxRetries: 3
  retryBackoff: "10s"

testdata:
  cacheRoot: "/data/oj/judge-cache"
  maxCacheBytes: 21474836480
  maxUnusedDuration: "72h"
  cleanupInterval: "1h"
  statsInterval: "5m"

gojudge:
  endpoint: "http://go-judge-sandbox:5050"
  authToken: ""

reporter:
  mode: "http"
  endpoint: "/judge/submissions/{submissionId}/events"

heartbeat:
  enabled: true
  endpoint: "/judge/nodes/heartbeat"
  interval: "30s"
EOF
  chmod 600 "${CONFIG_FILE}" || true
}

configure_interactive() {
  local node_type
  local node_name
  local max_concurrency
  local backend_url
  local rabbit_host
  local rabbit_port
  local rabbit_username
  local rabbit_password
  local rabbit_vhost
  local nacos_server=""
  local nacos_namespace=""
  local auth_code=""
  local remote_enabled="false"
  local formal_token_group=""
  local formal_token_data_id=""

  printf '节点类型：1) 正式节点  2) 临时节点\n' >&2
  while true; do
    node_type="$(prompt "请选择节点类型" "1")"
    case "${node_type}" in
      1|formal) node_type="formal"; break ;;
      2|temp) node_type="temp"; break ;;
      *) printf '请输入 1 或 2。\n' >&2 ;;
    esac
  done

  node_name="$(prompt "节点名称" "judge-node-01")"
  max_concurrency="$(prompt_int "最大并发任务数" "2")"
  backend_url="$(prompt "后端服务地址" "http://127.0.0.1:8800")"
  rabbit_host="$(prompt "RabbitMQ 地址" "127.0.0.1")"
  rabbit_port="$(prompt_int "RabbitMQ 端口" "5672")"
  rabbit_username="$(prompt "RabbitMQ 用户名" "hnieoj_judge")"
  rabbit_password="$(prompt_secret_required "RabbitMQ 密码")"
  rabbit_vhost="$(prompt "RabbitMQ vhost" "hnieoj")"

  if [[ "${node_type}" == "formal" ]]; then
    mkdir -p "${SECURITY_DIR}"
    if [[ ! -f "${PRIVATE_KEY_FILE}" ]]; then
      fail "正式节点要求私钥文件存在：${PRIVATE_KEY_FILE}"
    fi
    chmod 600 "${PRIVATE_KEY_FILE}" || true
    nacos_server="$(prompt "Nacos 地址" "http://127.0.0.1:8848")"
    nacos_namespace="$(prompt "Nacos namespace" "dev")"
    remote_enabled="true"
    formal_token_group="$(prompt "正式 Token Nacos Group" "HNIEOJ_SECRET_GROUP")"
    formal_token_data_id="$(prompt "正式 Token Nacos Data ID" "hnieoj-judge-formal-token.yaml")"
  else
    auth_code="$(prompt_required "临时节点授权码")"
  fi

  write_common_config "${node_name}" "${node_type}" "${max_concurrency}" "${backend_url}" \
    "${rabbit_host}" "${rabbit_port}" "${rabbit_username}" "${rabbit_password}" "${rabbit_vhost}" \
    "${auth_code}" "${nacos_server}" "${nacos_namespace}" "${remote_enabled}" \
    "${formal_token_group}" "${formal_token_data_id}"
  log "配置已写入：${CONFIG_FILE}"
}

write_compose() {
  cat > "${COMPOSE_FILE}" <<EOF
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
    ports:
      - "${GOJUDGE_PUBLIC_PORT}:5050"
    networks:
      - hnieoj-judge

  hnieoj-judge-node:
    image: ${IMAGE}
    restart: unless-stopped
    command:
      - /usr/local/bin/hnieoj-judge-node
      - -config
      - /etc/hnieoj/go-judge/config.yaml
    volumes:
      - ${CONFIG_FILE}:/etc/hnieoj/go-judge/config.yaml:ro
      - ${SECURITY_DIR}:/etc/hnieoj/judge-security:ro
      - ${CACHE_DIR}:/data/oj/judge-cache
    networks:
      - hnieoj-judge

networks:
  hnieoj-judge:
    driver: bridge
EOF
  chmod 600 "${COMPOSE_FILE}" || true
  log "Compose 已写入：${COMPOSE_FILE}"
}

configure() {
  prepare_environment
  configure_interactive
  write_compose
}

deploy() {
  prepare_environment
  if [[ ! -f "${CONFIG_FILE}" ]]; then
    configure_interactive
  else
    local answer
    printf '发现已有配置 %s，是否重新交互生成？[y/N]: ' "${CONFIG_FILE}"
    read_line answer
    case "${answer}" in
      y|Y|yes|YES) configure_interactive ;;
      *) log "使用已有配置：${CONFIG_FILE}" ;;
    esac
  fi
  write_compose
  build_image
  docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" up -d
  docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" ps
  log "查看日志：bash deploy/deploy-judge-node.sh logs"
}

usage() {
  cat <<EOF
用法：
  bash deploy/deploy-judge-node.sh [命令]

命令：
  deploy      交互生成配置、构建镜像并启动，默认命令
  configure   只交互生成 ${CONFIG_FILE}
  build       只构建镜像
  up          启动容器
  ps          查看容器状态
  logs        查看容器日志
  down        停止并移除容器
  help        显示帮助

也可以手动复制模板：
  cp deploy/config.formal.example.yaml ${CONFIG_FILE}
  cp deploy/config.temp.example.yaml ${CONFIG_FILE}
  vi ${CONFIG_FILE}
  bash deploy/deploy-judge-node.sh up
EOF
}

main() {
  local command="${1:-deploy}"
  case "${command}" in
    deploy) deploy ;;
    configure) configure ;;
    build) prepare_environment; build_image ;;
    up) prepare_environment; write_compose; docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" up -d ;;
    ps) docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" ps ;;
    logs) docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" logs -f --tail=200 ;;
    down) docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" down ;;
    help|-h|--help) usage ;;
    *) usage; fail "未知命令：${command}" ;;
  esac
}

main "$@"
