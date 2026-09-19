# HnieOJ go-judge 判题机

HnieOJ go-judge 基于 [criyle/go-judge](https://github.com/criyle/go-judge) 二次开发：保留上游沙箱执行能力，并新增 HnieOJ 判题节点 Agent、WebUI 管理控制台、统一 Ed25519 身份、WSS 任务通道、测试数据缓存与结果持久队列。

相关项目：

- 源仓库：[criyle/go-judge](https://github.com/criyle/go-judge)
- HnieOJ 后端：[haoran37/HnieOJ-backend](https://github.com/haoran37/HnieOJ-backend)
- HnieOJ 前端：[haoran37/HnieOJ](https://github.com/haoran37/HnieOJ)
- API 文档：[Apifox](https://s.apifox.cn/91edc2c6-6918-4179-9852-9ec3742377c8)

## 核心能力

- **统一身份**：formal 与 temp 两类节点都在本地生成真实随机 Ed25519 keypair 与 `enrollmentId`，在任何入网请求前原子持久化；重启复用同一身份，不消耗新 Bootstrap。
- **WSS 任务通道**：任务、租约、心跳与控制消息统一走 `wss://<host>/ws/judge/node`；HTTPS 只负责入网、签名测试数据下载与密钥轮换。不再有 HTTP 轮询领取。
- **短期授权**：服务端签发 15 分钟级 NODE_ACCESS；`AUTH_REFRESH` 在同一连接上续授权，不中断在途任务、不改变 `sessionEpoch`；token 过期可用有效密钥重新认证，无需重新注册。
- **重连恢复**：断线指数抖动退避重连；在途 attempt 通过 `RESUME_TASKS` 恢复，合法 resume 保持 attempt 不重复执行，被拒绝/过期立即取消。
- **结果确认**：终态先写入有界持久队列，收到 `TASK_RESULT_ACK` 后才删除；队列满或磁盘写失败会停止接新任务并显式失败，绝不丢结果。
- **自动密钥轮换**：`prepare`/`confirm`/`query` 两阶段，旧 key 签 HTTPS、新 key proof；崩溃/响应丢失可恢复，旧 key 在 grace 后清理，不永久保留。
- **WebUI 控制台**：默认只绑定 loopback，支持初始化管理员密码、一次性 Bootstrap 入网、启动/停止/重启、运行状态、指标、日志与测试数据缓存管理；只返回配置标志与公开身份，不返回私钥/Bootstrap/AccessToken。
- **判题执行**：内置 `go-judge` 沙箱，支持 C、C++17、Java 17、Python 3，判题模式 `default`/`spj`/`interactive`。

## 运行架构

节点访问三类地址：

- 后端 HTTPS（`hnieoj.baseUrl`）：入网挑战/注册、签名测试数据下载、密钥轮换。远程必须 HTTPS；仅 `localhost`/`127.0.0.1`/`::1` 允许明文 HTTP 用于本地开发，且不提供跳过证书校验的开关。
- 后端 WSS（`hnieoj.wssUrl`，留空自动推导为 `wss://<host>/ws/judge/node`）：任务、租约、心跳与控制。远程必须 `wss`。
- 本地 go-judge sandbox（`gojudge.endpoint`）：本地/内网 HTTP 端点，与后端 TLS 要求相互独立。

服务端是唯一权威：并发额度、授权期限、`sessionEpoch` 都由后端批准，节点上报不能提高额度。

## 节点身份与入网

### 首次入网

1. 管理员在后端签发一次性 Bootstrap（formal 或 temp，含 `authorizationUntil`、`maxConcurrency` 等策略）。
2. WebUI 中填写后端地址与一次性 Bootstrap；Bootstrap 明文以 0600 原子写入状态目录。
3. 节点启动时生成真实随机 Ed25519 keypair 与 `enrollmentId` 并原子持久化，然后请求挑战并用合同规范化字段签名注册。
4. 注册回复丢失时，节点用同一 `enrollmentId` + 公钥 + Bootstrap 摘要重试，服务端返回同一 node；绝不创建第二个 node。
5. 注册成功后节点删除 Bootstrap 文件，`nodeId`/`keyId` 写入身份文件。

### 身份文件与安全状态

- 身份文件默认 `/var/lib/hnieoj-judge-node/identity.json`（位于状态目录，容器部署时随卷持久化）。
- POSIX：目录 0700、文件 0600，原子写入 + fsync。Windows：使用 `golang.org/x/sys/windows` 设置 current-user-only DACL，而不是跳过权限断言。
- 私钥只保存在本地身份文件，绝不出现在日志、WebUI 响应或沙箱挂载中；AccessToken 只是短期授权，不是身份。

### 密钥轮换

- 默认每 30 天自动轮换一次，可用 `rotation.interval` 注入更短周期（测试）。
- `prepare` 由当前 ACTIVE 旧 key 通过签名 HTTPS 发起，新 key 提供 proof；`confirm` 由新 key 签名。
- 本地在调用服务端前先持久化 pending 新密钥；确认失败/响应丢失/重启都会恢复，绝不提前删除唯一可用密钥。
- 确认成功后旧 key 进入 5 分钟 grace（可配置），到期清理；节点随后 `AUTH_REFRESH` 让 token 绑定新 key。

## Docker 部署

默认镜像：

```text
haoran37/hnieoj-go-judge:latest
```

直接启动（宿主机只映射 loopback）：

```bash
docker run -d \
  --name hnieoj-judge-node \
  --restart unless-stopped \
  --privileged \
  --cgroupns=host \
  --shm-size=512m \
  -e HNIEOJ_WEB_ADDR=0.0.0.0:3723 \
  -p 127.0.0.1:3723:3723 \
  -v hnieoj-judge-state:/var/lib/hnieoj-judge-node \
  -v hnieoj-judge-cache:/data/oj/judge-cache \
  haoran37/hnieoj-go-judge:latest
```

> `--cgroupns=host`：upstream go-judge 沙箱在 Docker 默认 private cgroup namespace 下可能无法获得 cgroup 路径（`cgroup path empty`），因此容器需使用宿主机 cgroup 命名空间。该参数需在支持 cgroup v2 的 Linux + Docker 环境验证。

启动后通过 SSH 隧道或本机访问 `http://127.0.0.1:3723`，首次进入创建管理员密码，然后填写后端地址与一次性 Bootstrap 完成入网。

## 一键脚本

脚本负责拉取镜像、替换旧容器、挂载状态目录与缓存目录，并把 WebUI 端口只映射到宿主机 loopback；身份/Bootstrap/启动在 WebUI 中完成。

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/master/deploy/deploy-judge-node.sh -o /tmp/hnieoj-judge-node.sh && sudo bash /tmp/hnieoj-judge-node.sh deploy
```

指定镜像 tag 或宿主机端口：

```bash
sudo IMAGE_TAG=sha-xxxxxxx bash /tmp/hnieoj-judge-node.sh deploy
sudo WEBUI_HOST_PORT=8080 bash /tmp/hnieoj-judge-node.sh deploy
```

容器内 WebUI 通过 `HNIEOJ_WEB_ADDR` 显式设为 `0.0.0.0:3723` 以配合端口映射；脚本默认只映射 `127.0.0.1`。

## 开发测试脚本

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/develop/deploy/dev-build-run.sh -o /tmp/hnieoj-dev-build-run.sh && sudo bash /tmp/hnieoj-dev-build-run.sh deploy
```

该脚本默认构建 `haoran37/hnieoj-go-judge:dev-local` 并启动 `hnieoj-judge-node-dev` 容器；开发状态目录默认 `/tmp/hnieoj-judge-node-dev/state`，不会覆盖生产目录。

## 注意事项

- 不要把 go-judge 沙箱 HTTP 端口暴露到公网；WebUI 在容器内管理沙箱进程。
- 远程后端必须 HTTPS/WSS；不要为非回环地址配置明文。
- 状态目录必须持久化，否则身份密钥、配置、待确认结果会丢失；该目录权限应为 0700。
- 私钥绝不挂载进沙箱；WebUI 只显示公开身份与配置标志。
- 缓存目录建议持久化，避免大测试数据反复下载。
- 本仓库只提供本地/内网自测；生产 DNS、证书与防火墙验收需在真实环境另行完成。

## 本地开发

```bash
gofmt -l .
go vet ./...
go test -race -count=1 ./internal/hnieoj/... ./cmd/hnieoj-judge-node
go test -race -count=1 ./internal/hnieoj/testdata
GOOS=windows GOARCH=amd64 go build ./...
go build -o ./tmp/hnieoj-judge-node ./cmd/hnieoj-judge-node
docker build -f Dockerfile.hnieoj -t haoran37/hnieoj-go-judge:dev .
```

连接本地后端调试时，`hnieoj.baseUrl` 可填 `http://127.0.0.1:8800`；远程必须 HTTPS。
