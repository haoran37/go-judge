# HnieOJ Judge Node

`cmd/hnieoj-judge-node` adds an HnieOJ adapter around the existing go-judge sandbox service. It consumes submission tasks, downloads cached problem testdata, calls go-judge `/run`, and reports progress events back to HnieOJ.

## Architecture

- `internal/hnieoj/auth`: formal token polling/decryption and temp JWT exchange.
- `internal/hnieoj/testdata`: versioned testdata cache under `{cacheRoot}/problems/{problemId}`.
- `internal/hnieoj/runner`: HTTP client for go-judge `/run`, language commands, output comparison, and status mapping.
- `internal/hnieoj/reporter`: replaceable `http` and `log/mock` event reporters.
- `internal/hnieoj/mq`: RabbitMQ consumer with manual ACK/NACK.
- `internal/hnieoj/processor`: submission-level orchestration.

The original `cmd/go-judge` sandbox service remains unchanged. Run it locally or point `gojudge.endpoint` to an existing instance.

## Configuration

Start from `config.example.yaml`. Key environment overrides:

- `HNIEOJ_NODE_NAME`, `HNIEOJ_NODE_TYPE`, `HNIEOJ_NODE_MAX_CONCURRENCY`
- `HNIEOJ_BASE_URL`, `HNIEOJ_REQUEST_TIMEOUT`
- `HNIEOJ_FORMAL_PRIVATE_KEY_PATH`
- `HNIEOJ_NACOS_SERVER_ADDR`, `HNIEOJ_NACOS_NAMESPACE`, `HNIEOJ_FORMAL_TOKEN_NACOS_GROUP`, `HNIEOJ_FORMAL_TOKEN_NACOS_DATA_ID`
- `HNIEOJ_TEMP_AUTH_CODE`
- `HNIEOJ_RABBITMQ_HOST`, `HNIEOJ_RABBITMQ_PORT`, `HNIEOJ_RABBITMQ_USERNAME`, `HNIEOJ_RABBITMQ_PASSWORD`
- `HNIEOJ_TESTDATA_CACHE_ROOT`
- `HNIEOJ_GOJUDGE_ENDPOINT`, `HNIEOJ_GOJUDGE_AUTH_TOKEN`
- `HNIEOJ_REPORTER_MODE`, `HNIEOJ_REPORTER_ENDPOINT`

Formal nodes read the encrypted formal token from Nacos, decrypt `{rsa}Base64CipherText` with a local PKCS#8 PEM private key, and send `X-Judge-Token`. Temp nodes call `POST /api/judge/temp-token` and send `Authorization: Bearer ...`.

When `hnieoj-judge` starts and no active formal token exists, the backend initializes one automatically and publishes ciphertext to Nacos. Formal token rotation can also be initiated by the backend admin endpoint:

```http
POST /api/admin/judge/nodes/formal-token/rotate
```

The backend stores only the token hash, encrypts the new token with the formal public key, and publishes ciphertext to `hnieoj-judge-formal-token.yaml`. The node refreshes the Nacos ciphertext periodically and does not need to restart after rotation.

## Run

```bash
go build -o ./tmp/go-judge ./cmd/go-judge
./tmp/go-judge -http-addr=:5050

go build -o ./tmp/hnieoj-judge-node ./cmd/hnieoj-judge-node
./tmp/hnieoj-judge-node -config config.example.yaml
```

For local one-shot verification without RabbitMQ:

```bash
./tmp/hnieoj-judge-node -config config.example.yaml -fixture ./task.fixture.json
```

## Docker

Build the HnieOJ image:

```bash
docker build -f Dockerfile.hnieoj -t hnieoj/go-judge:dev .
```

Run the sandbox service:

```bash
docker run --rm --privileged --name go-judge-sandbox -p 5050:5050 hnieoj/go-judge:dev
```

Run the HnieOJ adapter with a mounted config:

```bash
docker run --rm --name hnieoj-judge-node \
  -v /etc/hnieoj/go-judge/config.yaml:/etc/hnieoj/go-judge/config.yaml:ro \
  -v /etc/hnieoj/judge-security:/etc/hnieoj/judge-security:ro \
  -v /data/oj/judge-cache:/data/oj/judge-cache \
  hnieoj/go-judge:dev \
  /usr/local/bin/hnieoj-judge-node -config /etc/hnieoj/go-judge/config.yaml
```

## Backend Contracts Required

RabbitMQ must publish JSON tasks to:

- Exchange: `hnieoj.judge.exchange`
- Queue: `hnieoj.judge.task`
- Routing key: `judge.submission.created`
- Dead letter exchange: `hnieoj.judge.dlx`
- Dead letter queue: `hnieoj.judge.task.dlq`
- Dead letter routing key: `judge.submission.created.dlq`
- ACK mode: manual
- Retry policy: retryable errors are republished to the task exchange with header `x-hnieoj-retry-count`; after `rabbitmq.maxRetries` the original message is rejected without requeue and enters the DLQ.

Task body must follow the documented submission contract and include `submissionId`, `judgeId`, `problemId`, `language`, `code`, resource limits, `dataVersion`, and judge flags.

Example:

```json
{
  "submissionId": "9d7bcf7f6f024e4d9cd63c3e85a5e39f",
  "judgeId": 123,
  "problemId": 1001,
  "problemCode": "P1000",
  "language": "cpp",
  "code": "#include <bits/stdc++.h>\nusing namespace std;\nint main(){int a,b;cin>>a>>b;cout<<a+b<<'\\n';}",
  "timeLimit": 1000,
  "memoryLimit": 256,
  "stackLimit": 128,
  "judgeMode": "default",
  "problemType": 0,
  "ioScore": 100,
  "isRemoveEndBlank": true,
  "dataVersion": 3,
  "contestId": 0,
  "createdAt": "2026-06-06T20:00:00+08:00"
}
```

The recommended event endpoint is:

```http
POST /judge/submissions/{submissionId}/events
Idempotency-Key: {submissionId}:{eventType}:{judgedCase}:{currentCase}
X-Judge-Token: {formal token}
```

Temp nodes use `Authorization: Bearer {jwt}`. The backend should update `judge`, upsert `judge_case`, and push `/topic/submissions/{submissionId}/progress`.

Heartbeat endpoint:

```http
POST /judge/nodes/heartbeat
```

Formal deployment examples enable heartbeat by default. The backend uses it to maintain node online status, current running task count, max concurrency, CPU cores, judge node version, testdata cache usage, cached problem count, and free disk space of the cache mount. Cache and disk stats are sampled about every 5 minutes to avoid scanning the cache directory on every heartbeat.

## Runtime Config

Formal deployment examples can load non-sensitive runtime settings from Nacos:

```yaml
remoteConfig:
  enabled: true
  nacos:
    serverAddr: "http://106.54.177.244:8848"
    namespace: "dev"
    group: "HNIEOJ_JUDGE_GROUP"
    dataId: "hnieoj-judge-node.yaml"
```

Use this remote config for shared operational settings such as `testdata.maxCacheBytes`, `testdata.maxUnusedDuration`, `testdata.cleanupInterval`, `testdata.statsInterval`, `heartbeat.interval`, and RabbitMQ retry/prefetch values. Do not put node names, private key paths, RabbitMQ passwords, temp auth codes, or other node-private/sensitive values in this file.

The testdata cleaner removes cached problem data that has not been used for `maxUnusedDuration`, then evicts the least recently used cached problems until total cache usage is below `maxCacheBytes`. Set either value to `0` to disable that cleanup condition.

## CLI Setup

The judge node binary provides setup commands:

```bash
hnieoj-judge-node init
hnieoj-judge-node auth-exchange -config /etc/hnieoj/go-judge/config.yaml
hnieoj-judge-node config-validate -config /etc/hnieoj/go-judge/config.yaml
hnieoj-judge-node doctor -config /etc/hnieoj/go-judge/config.yaml
```

`init` interactively writes `config.yaml` and `docker-compose.yml`. Formal nodes require the private key file at `/etc/hnieoj/judge-security/judge_formal_private.pem`. Temp nodes should run `auth-exchange`, enter the one-time auth code, and store the returned JWT in `/etc/hnieoj/go-judge/temp-token.json` with `0600` permissions. The auth code is not persisted by the CLI.

For Docker-based initialization, run the setup command in a one-shot container with the config and security directories mounted, then start the generated compose file.

The repository also provides a wrapper script for server deployment:

```bash
bash deploy/deploy-judge-node.sh
```

The script builds the local image, prepares directories, creates or reuses the Docker network, runs the interactive CLI initializer, optionally runs `auth-exchange`, and starts the generated compose file. It is only a deployment wrapper; configuration validation and temp token exchange still use `hnieoj-judge-node` CLI.

## Validation Checklist

1. Formal token is fetched from Nacos and decrypts with RSA OAEP SHA-256.
2. Temp token exchange returns a usable JWT.
3. Testdata download handles `304 Not Modified`.
4. ZIP extraction rejects paths, directories, and non `.in/.out` files.
5. Fixture or RabbitMQ task reaches go-judge `/run`.
6. C++17, C, Java 17, and Python 3 commands are mapped.
7. AC, WA, CE, RE, TLE, MLE, and System Error events are reported.
8. Logs do not print tokens, auth codes, or private key contents.

## Known Limits

Only `judgeMode: default` text comparison is implemented. SPJ and interactive judging are rejected as System Error until backend and runner contracts are defined. Java support currently caches `Main.class`; multi-file Java submissions need a later artifact packaging step.
