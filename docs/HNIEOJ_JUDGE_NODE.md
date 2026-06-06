# HnieOJ Judge Node

`cmd/hnieoj-judge-node` adds an HnieOJ adapter around the existing go-judge sandbox service. It consumes submission tasks, downloads cached problem testdata, calls go-judge `/run`, and reports progress events back to HnieOJ.

## Architecture

- `internal/hnieoj/auth`: formal token decryption and temp JWT exchange.
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
- `HNIEOJ_FORMAL_ENCRYPTED_TOKEN`, `HNIEOJ_FORMAL_PRIVATE_KEY_PATH`
- `HNIEOJ_TEMP_AUTH_CODE`
- `HNIEOJ_RABBITMQ_HOST`, `HNIEOJ_RABBITMQ_PORT`, `HNIEOJ_RABBITMQ_USERNAME`, `HNIEOJ_RABBITMQ_PASSWORD`
- `HNIEOJ_TESTDATA_CACHE_ROOT`
- `HNIEOJ_GOJUDGE_ENDPOINT`, `HNIEOJ_GOJUDGE_AUTH_TOKEN`
- `HNIEOJ_REPORTER_MODE`, `HNIEOJ_REPORTER_ENDPOINT`

Formal nodes decrypt `{rsa}Base64CipherText` with a local PKCS#8 PEM private key and send `X-Judge-Token`. Temp nodes call `POST /api/judge/temp-token` and send `Authorization: Bearer ...`.

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

## Backend Contracts Required

RabbitMQ must publish JSON tasks to:

- Exchange: `hnieoj.judge.exchange`
- Queue: `hnieoj.judge.task`
- Routing key: `judge.submission.created`
- ACK mode: manual

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

Optional heartbeat endpoint:

```http
POST /judge/nodes/heartbeat
```

## Validation Checklist

1. Formal token decrypts with RSA OAEP SHA-256.
2. Temp token exchange returns a usable JWT.
3. Testdata download handles `304 Not Modified`.
4. ZIP extraction rejects paths, directories, and non `.in/.out` files.
5. Fixture or RabbitMQ task reaches go-judge `/run`.
6. C++17, C, Java 17, and Python 3 commands are mapped.
7. AC, WA, CE, RE, TLE, MLE, and System Error events are reported.
8. Logs do not print tokens, auth codes, or private key contents.

## Known Limits

Only `judgeMode: default` text comparison is implemented. SPJ and interactive judging are rejected as System Error until backend and runner contracts are defined. Java support currently caches `Main.class`; multi-file Java submissions need a later artifact packaging step.
