#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-haoran37/hnieoj-go-judge}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
IMAGE="${IMAGE:-${IMAGE_REPOSITORY}:${IMAGE_TAG}}"
PROJECT_NAME="${PROJECT_NAME:-hnieoj-judge-node}"
CONFIG_DIR="${CONFIG_DIR:-/etc/hnieoj/go-judge}"
SECURITY_DIR="${SECURITY_DIR:-/etc/hnieoj/judge-security}"
CACHE_DIR="${CACHE_DIR:-/data/oj/judge-cache}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.yaml}"
COMPOSE_FILE="${COMPOSE_FILE:-${CONFIG_DIR}/compose.yaml}"
PRIVATE_KEY_FILE="${PRIVATE_KEY_FILE:-${SECURITY_DIR}/judge_formal_private.pem}"

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

check_linux() {
  local kernel_name=""
  local os_pretty=""
  kernel_name="$(uname -s 2>/dev/null || true)"
  [[ "${kernel_name}" == "Linux" ]] || fail "当前脚本仅支持 Linux 系统，当前系统为：${kernel_name:-未知}"
  if [[ -r /etc/os-release ]]; then
    # 读取发行版名称用于错误提示；不按具体发行版做硬限制。
    # shellcheck disable=SC1091
    . /etc/os-release
    os_pretty="${PRETTY_NAME:-Linux}"
  else
    os_pretty="Linux"
  fi
  [[ -n "${os_pretty}" ]] || fail "无法识别 Linux 发行版"
}

check_default_path_permissions() {
  local path
  if [[ "${EUID}" -eq 0 ]]; then
    return
  fi
  for path in "${CONFIG_DIR}" "${SECURITY_DIR}" "${CACHE_DIR}"; do
    case "${path}" in
      /etc/*|/data/*|/var/*)
        fail "默认部署目录 ${path} 需要 root 权限；请使用 sudo 执行，或通过 CONFIG_DIR/SECURITY_DIR/CACHE_DIR 指定其他目录"
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
    warn "该项不能为空"
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

json_quote() {
  local value="${1:-}"
  [[ "${value}" != *$'\n'* ]] || fail "JSON 字段不能包含换行"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "${value}"
}

validate_node_type() {
  case "$1" in
    formal|temp) return 0 ;;
    *) fail "不支持的节点类型：$1" ;;
  esac
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

default_node_name() {
  local host
  local suffix
  host="$(hostname -s 2>/dev/null || hostname 2>/dev/null || printf 'node')"
  host="${host//[^a-zA-Z0-9_.-]/-}"
  [[ -n "${host}" ]] || host="node"
  suffix="$(date '+%Y%m%d%H%M%S')"
  printf 'judge-%s-%s' "${host}" "${suffix}"
}

prepare_dirs() {
  mkdir -p "${CONFIG_DIR}" "${SECURITY_DIR}" "${CACHE_DIR}"
  chmod 700 "${CONFIG_DIR}" "${SECURITY_DIR}" 2>/dev/null || true
  chmod 755 "${CACHE_DIR}" 2>/dev/null || true
}

yaml_value() {
  local section="$1"
  local key="$2"
  local file="${3:-${CONFIG_FILE}}"
  awk -v section="${section}" -v key="${key}" '
    function trim(s) {
      gsub(/^[ \t]+|[ \t]+$/, "", s)
      return s
    }
    function unquote(s) {
      s = trim(s)
      if ((substr(s, 1, 1) == "\"" && substr(s, length(s), 1) == "\"") ||
          (substr(s, 1, 1) == "'"'"'" && substr(s, length(s), 1) == "'"'"'")) {
        s = substr(s, 2, length(s) - 2)
      }
      return s
    }
    /^[^ \t#][^:]*:[ \t]*($|#)/ {
      line = $0
      sub(/[ \t]*#.*/, "", line)
      split(line, parts, ":")
      current = trim(parts[1])
      next
    }
    current == section {
      line = $0
      sub(/[ \t]*#.*/, "", line)
      pattern = "^[ \t]+" key ":[ \t]*"
      if (line ~ pattern) {
        sub(pattern, "", line)
        print unquote(line)
        exit
      }
    }
  ' "${file}"
}

yaml_list_value() {
  local section="$1"
  local key="$2"
  local file="${3:-${CONFIG_FILE}}"
  awk -v section="${section}" -v key="${key}" '
    function trim(s) {
      gsub(/^[ \t]+|[ \t]+$/, "", s)
      return s
    }
    /^[^ \t#][^:]*:[ \t]*($|#)/ {
      line = $0
      sub(/[ \t]*#.*/, "", line)
      split(line, parts, ":")
      current = trim(parts[1])
      in_list = 0
      next
    }
    current == section {
      line = $0
      sub(/[ \t]*#.*/, "", line)
      if (line ~ "^[ \t]+" key ":[ \t]*$") {
        in_list = 1
        next
      }
      if (in_list && line ~ "^[ \t]+-[ \t]*") {
        sub("^[ \t]+-[ \t]*", "", line)
        item = trim(line)
        if (item != "") {
          if (out != "") out = out ","
          out = out item
        }
        next
      }
      if (in_list && line !~ "^[ \t]+") {
        in_list = 0
      }
    }
    END {
      print out
    }
  ' "${file}"
}

check_source_tree() {
  [[ -f "${SOURCE_DIR}/Dockerfile.hnieoj" ]] || fail "在 ${SOURCE_DIR} 下未找到 Dockerfile.hnieoj；远程下载脚本只支持拉取镜像部署，本地构建请在源码仓库中执行"
}

preflight() {
  check_linux
  check_default_path_permissions
  check_docker
  prepare_dirs
}

check_tcp_connectivity() {
  local label="$1"
  local host="$2"
  local port="$3"
  [[ -n "${host}" ]] || fail "${label} 主机为空，请检查配置"
  [[ "${port}" =~ ^[0-9]+$ ]] || fail "${label} 端口无效：${port}"
  if command -v timeout >/dev/null 2>&1; then
    timeout 5 bash -c ': >/dev/tcp/$1/$2' _ "${host}" "${port}" >/dev/null 2>&1 || fail "${label} 不可连接：${host}:${port}"
  else
    bash -c ': >/dev/tcp/$1/$2' _ "${host}" "${port}" >/dev/null 2>&1 || fail "${label} 不可连接：${host}:${port}"
  fi
  log "${label} 连通性正常：${host}:${port}"
}

check_backend_connectivity() {
  local backend_url
  backend_url="$(yaml_value hnieoj baseUrl)"
  [[ -n "${backend_url}" ]] || fail "HnieOJ 后端地址为空，请检查 hnieoj.baseUrl"
  require_command curl
  curl -k -sS -o /dev/null --connect-timeout 5 --max-time 10 "${backend_url}" || fail "HnieOJ 后端不可连接：${backend_url}"
  log "HnieOJ 后端连通性正常：${backend_url}"
}

check_rabbitmq_connectivity() {
  local host
  local port
  host="$(yaml_value rabbitmq host)"
  port="$(yaml_value rabbitmq port)"
  check_tcp_connectivity "RabbitMQ" "${host}" "${port}"
}

check_gojudge_status() {
  if docker_compose ps --status running go-judge-sandbox >/dev/null 2>&1; then
    if docker_compose ps --status running go-judge-sandbox | grep -q 'go-judge-sandbox'; then
      log "go-judge sandbox 容器正在运行"
      return
    fi
  fi
  warn "go-judge sandbox 容器尚未运行；首次部署时会在 docker compose up 后启动"
}

print_deployment_summary() {
  local node_name
  local node_type
  local supported_modes
  local backend_url
  local rabbit_host
  local rabbit_port
  local heartbeat_enabled
  local heartbeat_interval
  node_name="$(yaml_value node name)"
  node_type="$(yaml_value node type)"
  supported_modes="$(yaml_list_value node supportedJudgeModes)"
  backend_url="$(yaml_value hnieoj baseUrl)"
  rabbit_host="$(yaml_value rabbitmq host)"
  rabbit_port="$(yaml_value rabbitmq port)"
  heartbeat_enabled="$(yaml_value heartbeat enabled)"
  heartbeat_interval="$(yaml_value heartbeat interval)"
  [[ -n "${supported_modes}" ]] || supported_modes="default"
  log "部署摘要"
  printf '  节点名称：%s\n' "${node_name:-未知}"
  printf '  节点类型：%s\n' "${node_type:-未知}"
  printf '  判题模式：%s\n' "${supported_modes}"
  printf '  镜像地址：%s\n' "${IMAGE}"
  printf '  后端地址：%s\n' "${backend_url:-未知}"
  printf '  RabbitMQ：%s:%s\n' "${rabbit_host:-未知}" "${rabbit_port:-未知}"
  printf '  心跳配置：enabled=%s interval=%s\n' "${heartbeat_enabled:-未知}" "${heartbeat_interval:-未知}"
}

parse_temp_token_response() {
  local response_file="$1"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "${response_file}" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    payload = json.load(f)

code = payload.get("code")
data = payload.get("data") or {}
token = data.get("token") or ""
if code != 200 or not token:
    msg = payload.get("msg") or "空 token"
    raise SystemExit(f"临时令牌兑换失败：{msg}")

values = [
    token,
    data.get("tokenType") or "Bearer",
    data.get("nodeId") or "",
    data.get("tokenId") or "",
    data.get("expireTime") or "",
]
print("\t".join(values))
PY
    return
  fi

  if command -v jq >/dev/null 2>&1; then
    jq -r '
      if (.code != 200 or ((.data.token // "") == "")) then
        error("临时令牌兑换失败：" + (.msg // "空 token"))
      else
        [
          .data.token,
          (.data.tokenType // "Bearer"),
          (.data.nodeId // ""),
          (.data.tokenId // ""),
          (.data.expireTime // "")
        ] | @tsv
      end
    ' "${response_file}"
    return
  fi

  fail "临时令牌兑换需要 python3 或 jq 来解析后端 JSON 响应"
}

exchange_temp_token() {
  local backend_url="$1"
  local node_name="$2"
  local auth_code="$3"
  local endpoint="${backend_url%/}/api/judge/temp-token"
  local request_body
  local response_file
  local http_code
  local parsed

  require_command curl
  request_body='{"authCode":'"$(json_quote "${auth_code}")"',"nodeName":'"$(json_quote "${node_name}")"'}'
  response_file="$(mktemp "${CONFIG_DIR}/temp-token-response.XXXXXX")"

  if ! http_code="$(curl -sS --connect-timeout 10 --max-time 30 \
    -o "${response_file}" \
    -w '%{http_code}' \
    -H 'Content-Type: application/json' \
    -X POST \
    --data "${request_body}" \
    "${endpoint}")"; then
    rm -f "${response_file}"
    warn "请求 ${endpoint} 失败"
    return 1
  fi

  if [[ ! "${http_code}" =~ ^2[0-9][0-9]$ ]]; then
    warn "临时令牌兑换返回 HTTP ${http_code}: $(tr -d '\r\n' < "${response_file}")"
    rm -f "${response_file}"
    return 1
  fi

  if ! parsed="$(parse_temp_token_response "${response_file}")"; then
    rm -f "${response_file}"
    return 1
  fi
  rm -f "${response_file}"

  IFS=$'\t' read -r TEMP_JWT TEMP_TOKEN_TYPE TEMP_NODE_ID TEMP_TOKEN_ID TEMP_EXPIRE_TIME <<< "${parsed}"
  [[ -n "${TEMP_JWT}" ]] || return 1
  log "临时令牌兑换成功，JWT 过期时间：${TEMP_EXPIRE_TIME:-未知}"
}

write_config_file() {
  local node_name="$1"
  local node_type="$2"
  local max_concurrency="$3"
  local supported_modes="$4"
  local backend_url="$5"
  local rabbit_host="$6"
  local rabbit_port="$7"
  local rabbit_username="$8"
  local rabbit_password="$9"
  local rabbit_vhost="${10}"
  local auth_code="${11}"
  local temp_jwt="${12}"
  local temp_token_type="${13}"
  local temp_node_id="${14}"
  local temp_token_id="${15}"
  local temp_expire_time="${16}"
  local nacos_server="${17}"
  local nacos_namespace="${18}"
  local remote_enabled="${19}"
  local formal_token_group="${20}"
  local formal_token_data_id="${21}"

  local tmp_file
  tmp_file="$(mktemp "${CONFIG_DIR}/config.yaml.tmp.XXXXXX")"

  {
    cat <<EOF
# HnieOJ 判题节点运行配置。
# 由 deploy/deploy-judge-node.sh 生成。真实密码、授权码和私钥不要提交到仓库。

node:
  # 节点名称，建议全局唯一。
  name: $(yaml_quote "${node_name}")
  # 节点类型：formal 为正式长期节点，temp 为临时节点。
  type: $(yaml_quote "${node_type}")
  # 最大并发判题任务数。
  maxConcurrency: ${max_concurrency}
  # 本节点支持的判题模式。确认后端和题目协议闭环后再开启 spj/interactive。
  supportedJudgeModes:
EOF
    write_modes_yaml "${supported_modes}"
    cat <<EOF

remoteConfig:
  # 是否从 Nacos 加载非敏感运行参数。
  enabled: ${remote_enabled}
  nacos:
    serverAddr: $(yaml_quote "${nacos_server}")
    namespace: $(yaml_quote "${nacos_namespace}")
    group: "HNIEOJ_JUDGE_GROUP"
    dataId: "hnieoj-judge-node.yaml"

hnieoj:
  # HnieOJ 后端服务地址。
  baseUrl: $(yaml_quote "${backend_url}")
  # 节点访问后端接口的超时时间。
  requestTimeout: "30s"
  formalToken:
    # formal 节点私钥路径。容器内路径由部署脚本挂载。
    privateKeyPath: $(yaml_quote "${PRIVATE_KEY_FILE}")
    cipherAlgorithm: "RSA/ECB/OAEPWithSHA-256AndMGF1Padding"
    # formal token 密文刷新间隔。
    refreshInterval: "30s"
    nacos:
      serverAddr: $(yaml_quote "${nacos_server}")
      namespace: $(yaml_quote "${nacos_namespace}")
      group: $(yaml_quote "${formal_token_group}")
      dataId: $(yaml_quote "${formal_token_data_id}")
  tempToken:
    # temp 节点授权码。脚本会先用它兑换 JWT，成功后再启动容器。
    authCode: $(yaml_quote "${auth_code}")
    # 预兑换得到的 JWT。容器启动时优先使用该值作为首次凭证。
    jwt: $(yaml_quote "${temp_jwt}")
    # JWT 类型，通常为 Bearer。
    tokenType: $(yaml_quote "${temp_token_type}")
    # 后端返回的临时节点 ID。
    nodeId: $(yaml_quote "${temp_node_id}")
    # 后端返回的临时 token ID。
    tokenId: $(yaml_quote "${temp_token_id}")
    # JWT 过期时间。节点会在过期前使用 authCode 刷新。
    expireTime: $(yaml_quote "${temp_expire_time}")

rabbitmq:
  # RabbitMQ 连接和判题任务队列配置。
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
  # 预取数量通常与 maxConcurrency 保持一致。
  prefetch: ${max_concurrency}
  # 可重试错误的最大重试次数和退避间隔。
  maxRetries: 3
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
  # go-judge sandbox 服务地址。
  endpoint: "http://go-judge-sandbox:5050"
  # 如果 sandbox 开启 -auth-token，则填写对应 token。
  authToken: ""

reporter:
  # http 表示上报后端；log/mock 适合本地调试。
  mode: "http"
  endpoint: "/judge/submissions/{submissionId}/events"

heartbeat:
  # 生产环境建议开启心跳，间隔不要设置为 1 秒级别。
  enabled: true
  endpoint: "/judge/nodes/heartbeat"
  interval: "30s"
EOF
  } > "${tmp_file}"

  chmod 600 "${tmp_file}" 2>/dev/null || true
  mv "${tmp_file}" "${CONFIG_FILE}"
  log "已写入配置文件：${CONFIG_FILE}"
}

init_config() {
  check_linux
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
  local rabbit_host
  local rabbit_port
  local rabbit_username
  local rabbit_password
  local rabbit_vhost
  local temp_jwt=""
  local temp_token_type=""
  local temp_node_id=""
  local temp_token_id=""
  local temp_expire_time=""
  local nacos_server=""
  local nacos_namespace=""
  local auth_code=""
  local remote_enabled="false"
  local formal_token_group=""
  local formal_token_data_id=""

  node_name="$(prompt "节点名称" "$(default_node_name)")"
  max_concurrency="$(prompt_positive_int "最大并发判题任务数" "2")"
  supported_modes="$(normalize_modes_csv "$(prompt "支持的判题模式" "default")")"
  backend_url="$(prompt "HnieOJ 后端基础地址" "http://127.0.0.1:8800")"
  rabbit_host="$(prompt "RabbitMQ 主机" "127.0.0.1")"
  rabbit_port="$(prompt_positive_int "RabbitMQ 端口" "5672")"
  rabbit_username="$(prompt "RabbitMQ 用户名" "hnieoj_judge")"
  rabbit_password="$(prompt_secret_required "RabbitMQ 密码")"
  rabbit_vhost="$(prompt "RabbitMQ 虚拟主机" "hnieoj")"

  if [[ "${node_type}" == "formal" ]]; then
    nacos_server="$(prompt "Nacos 服务地址" "http://127.0.0.1:8848")"
    nacos_namespace="$(prompt "Nacos 命名空间" "dev")"
    remote_enabled="$(prompt "是否启用 Nacos 远程运行配置，填写 true 或 false" "true")"
    case "${remote_enabled}" in
      true|false) ;;
      *) fail "远程运行配置只能填写 true 或 false" ;;
    esac
    formal_token_group="$(prompt "formal token 的 Nacos group" "HNIEOJ_SECRET_GROUP")"
    formal_token_data_id="$(prompt "formal token 的 Nacos dataId" "hnieoj-judge-formal-token.yaml")"
    if [[ ! -f "${PRIVATE_KEY_FILE}" ]]; then
      warn "formal 私钥尚不存在：${PRIVATE_KEY_FILE}"
      warn "请在启动节点前复制私钥到该路径"
    else
      chmod 600 "${PRIVATE_KEY_FILE}" 2>/dev/null || true
    fi
  else
    while true; do
      auth_code="$(prompt_secret_required "临时节点授权码")"
      if exchange_temp_token "${backend_url}" "${node_name}" "${auth_code}"; then
        temp_jwt="${TEMP_JWT}"
        temp_token_type="${TEMP_TOKEN_TYPE}"
        temp_node_id="${TEMP_NODE_ID}"
        temp_token_id="${TEMP_TOKEN_ID}"
        temp_expire_time="${TEMP_EXPIRE_TIME}"
        break
      fi
      warn "临时授权码无效或已过期，请重新输入"
    done
  fi

  write_config_file "${node_name}" "${node_type}" "${max_concurrency}" "${supported_modes}" "${backend_url}" \
    "${rabbit_host}" "${rabbit_port}" "${rabbit_username}" "${rabbit_password}" "${rabbit_vhost}" \
    "${auth_code}" "${temp_jwt}" "${temp_token_type}" "${temp_node_id}" "${temp_token_id}" "${temp_expire_time}" \
    "${nacos_server}" "${nacos_namespace}" "${remote_enabled}" \
    "${formal_token_group}" "${formal_token_data_id}"
}

render_compose() {
  check_linux
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
      - "${SECURITY_DIR}:/etc/hnieoj/judge-security:ro"
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
  if grep -Eq 'type:[[:space:]]*"?formal"?' "${CONFIG_FILE}" && [[ ! -f "${PRIVATE_KEY_FILE}" ]]; then
    fail "formal 节点私钥缺失：${PRIVATE_KEY_FILE}"
  fi
  docker_compose config >/dev/null
  check_backend_connectivity
  check_rabbitmq_connectivity
  check_gojudge_status
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
  check_gojudge_status
  print_deployment_summary
  log "部署完成"
}

usage() {
  cat <<EOF
用法：
  bash deploy/deploy-judge-node.sh <command>

命令：
  deploy        配置缺失时先初始化，然后渲染 Compose、拉取镜像并重建服务。
  init          交互式写入 ${CONFIG_FILE}。
  render        按当前环境变量和配置路径渲染 ${COMPOSE_FILE}。
  doctor        检查 Docker、配置文件、Compose 文件和 formal 节点私钥。
  pull          从 Docker Hub 拉取 ${IMAGE}。
  build         在源码仓库中基于 Dockerfile.hnieoj 构建 ${IMAGE}。
  up            渲染 Compose 并启动服务。
  restart       重启服务。
  ps            查看服务状态。
  logs          跟随日志；可在命令后追加服务名。
  down          停止并删除服务。
  help          显示帮助。

常用环境变量：
  IMAGE_REPOSITORY=${IMAGE_REPOSITORY}
  IMAGE_TAG=${IMAGE_TAG}
  IMAGE=${IMAGE}
  PROJECT_NAME=${PROJECT_NAME}
  CONFIG_DIR=${CONFIG_DIR}
  SECURITY_DIR=${SECURITY_DIR}
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
