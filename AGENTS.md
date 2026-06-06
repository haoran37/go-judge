# Repository Guidelines

## Project Structure & Module Organization

This is a Go 1.25 project for the `go-judge` service and related tools. Main binaries live under `cmd/`: `cmd/go-judge` is the HTTP/gRPC service, while `cmd/go-judge-shell`, `cmd/go-judge-ffi`, `cmd/go-judge-grpc-proxy`, and `cmd/go-judge-init` provide supporting executables. Core execution logic is split across `env/` for platform-specific sandbox environments, `envexec/` for command/file execution helpers, `worker/` for worker orchestration, and `filestore/` for cached file storage. Protobuf definitions and generated Go files are in `pb/`. Unit tests sit beside source files as `*_test.go`; broader end-to-end tests are in `integration_test/`.

## Build, Test, and Development Commands

- `go test ./...`: run normal package tests.
- `go test -tags integration -v -count=1 ./integration_test`: run integration tests against a locally running `go-judge` instance.
- `go test -tags integration -cpu 1 -bench . ./integration_test`: run integration benchmarks with controlled parallelism.
- `go build -o ./tmp/go-judge ./cmd/go-judge`: build the main service binary.
- `air`: run the live-reload development loop defined in `.air.toml`; it builds `./cmd/go-judge` and starts it with REST, gRPC, debug, and metrics enabled.
- `docker run -it --rm --privileged --shm-size=256m -p 5050:5050 --name=go-judge criyle/go-judge`: run the published service image.

## Coding Style & Naming Conventions

Use standard Go formatting: run `gofmt` or `go fmt ./...` before committing. Package names are short lowercase identifiers, matching existing directories such as `envexec`, `filestore`, and `worker`. Keep platform-specific files on the established suffix pattern: `_linux.go`, `_windows.go`, `_darwin.go`, or `_others.go`. Do not hand-edit generated protobuf files; update `.proto` sources and regenerate instead.

## Testing Guidelines

Prefer table-driven unit tests colocated with the code they exercise. Name tests `TestXxx` and benchmarks `BenchmarkXxx`. Integration tests require a running local service and the `integration` build tag, so keep fast unit tests separate from environment-dependent coverage.

## Commit & Pull Request Guidelines

Recent history uses Conventional Commits, for example `fix(envexec): avoid potential leak`, `feat(pipe): bind cpuset for pipe proxy`, and `chore(deps): update go-sandbox`. Use the same `type(scope): summary` style. Pull requests should explain the behavior change, list tests run, link related issues, and call out platform or sandboxing impact. Include API or protobuf compatibility notes when touching `pb/` or public request/response models.

## Security & Configuration Tips

Sandbox behavior depends on OS support, privileges, cgroups, and container settings. Avoid weakening isolation defaults in `env/` or `mount.yaml` without documenting the risk. Never commit secrets; use runtime flags such as `-auth-token`, `-http-addr`, and `-grpc-addr` for local configuration.
