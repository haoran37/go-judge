# HNieOJ go-judge

Docker image for HNieOJ judge nodes. It contains:

- `go-judge`: sandbox service.
- `hnieoj-judge-node`: HNieOJ adapter for authentication, RabbitMQ task consumption, testdata cache, go-judge execution, heartbeat, and event reporting.
- C, C++17, Java 17, and Python 3 runtime toolchains.

Image repository:

```text
haoran37/hnieoj-go-judge
```

Tags:

- `latest`: latest image built from the `master` branch.
- `sha-xxxxxxx`: immutable image for a specific Git commit.

## Recommended Deployment

Use the deployment wrapper from the repository:

```bash
IMAGE_TAG=latest bash deploy/deploy-judge-node.sh deploy
```

Use a fixed commit tag for safer production rollout:

```bash
IMAGE_TAG=sha-xxxxxxx bash deploy/deploy-judge-node.sh deploy
```

The script will:

- create or reuse `/etc/hnieoj/go-judge/config.yaml`;
- render `/etc/hnieoj/go-judge/compose.yaml`;
- pull `haoran37/hnieoj-go-judge:<tag>`;
- validate configuration;
- recreate old containers with the new image.

## Manual Deployment Notes

Manual `docker run` or handwritten Compose deployment is possible, but you must handle these details yourself:

- Run `go-judge` with `--privileged`.
- Keep `-file-timeout` enabled, for example `-file-timeout=30m`, to avoid filestore accumulation.
- Mount `mount.yaml` into the sandbox container or use the image default path.
- Mount the HNieOJ config file into the judge-node container.
- Mount formal-node private keys read-only when using formal nodes.
- Mount a persistent testdata cache directory.
- Do not expose the go-judge HTTP port to the public Internet.
- For temp nodes, validate `authCode` before starting the container.

The deployment script performs temp-node preflight token exchange:

```http
POST /api/judge/temp-token
```

It writes the returned JWT into `hnieoj.tempToken.jwt` before the container starts. If you bypass the script, invalid temp auth codes may only be discovered after the container starts unless you implement the same preflight step.

## Example

```bash
docker pull haoran37/hnieoj-go-judge:latest

IMAGE=haoran37/hnieoj-go-judge:latest \
bash deploy/deploy-judge-node.sh deploy
```

## Judge Modes

The node supports:

- `default`
- `spj`
- `interactive`

Production nodes advertise enabled modes through heartbeat `supportedJudgeModes`. Keep the default as `default` unless the backend and problem data have been verified for SPJ or interactive judging.

## Documentation

See repository docs:

- `docs/HNIEOJ_DEPLOYMENT_MANUAL_CN.md`
- `docs/HNIEOJ_SPJ_INTERACTIVE_CONTRACT.md`
