# HnieOJ go-judge 判题机

HnieOJ go-judge 是基于 [criyle/go-judge](https://github.com/criyle/go-judge) 的二次开发版本。它保留上游沙箱执行能力，并新增 HnieOJ 判题节点适配层，用于连接后端内嵌判题任务网关、节点认证、测试数据缓存、心跳和判题事件上报。

这个仓库面向 HnieOJ 生产部署，不是上游 go-judge 的通用替代发行版。

## 相关项目

- 源仓库：[criyle/go-judge](https://github.com/criyle/go-judge)
- HnieOJ 后端：[HnieOJ 后端仓库](https://github.com/haoran37/HNieOJ-backend)
- HnieOJ 前端：[HnieOJ 前端仓库](https://github.com/haoran37/HNieOJ)
- API 文档：[Apifox](https://s.apifox.cn/91edc2c6-6918-4179-9852-9ec3742377c8)

## 核心能力

- 沙箱执行：通过 `go-judge` 在受限环境中编译、运行和采集程序结果。
- 判题节点：`hnieoj-judge-node` 通过后端 HTTPS 网关有界领取判题任务并上报事件，不再消费 RabbitMQ，也不连接 Redis/Nacos。
- 节点认证：支持 `formal` 正式节点和 `temp` 临时节点，运行期统一使用逐节点 Bearer 凭证。
- 任务租约：按领取响应续租并在失去所有权时取消任务，避免旧节点继续上报结果。
- 测试数据缓存：支持缓存容量、未使用时间、定时清理和心跳统计采样；首次下载携带任务资格字段。
- 节点心跳：上报节点在线状态、并发能力、磁盘/缓存统计、支持的判题模式与 draining 状态。
- 判题模式：支持 `default`、`spj`、`interactive`，生产默认只开启 `default`。
- 语言环境：Docker 镜像内置 C、C++17、Java 17、Python 3 工具链。

## 运行架构

节点只访问两个地址：

- 后端网关（`hnieoj.baseUrl`）：认证、领取/续租、测试数据、事件上报、心跳。远程后端必须使用 HTTPS；仅 `localhost`/`127.0.0.1`/`::1` 允许明文 HTTP 用于本地开发，且不提供跳过证书校验的开关。
- 本地 go-judge sandbox（`gojudge.endpoint`）：本地或内网 HTTP 端点，与后端 HTTPS 要求相互独立。

节点不要求 RabbitMQ、Nacos 或 Redis 凭证。

## 镜像发布

Docker Hub 仓库：

```text
haoran37/hnieoj-go-judge
```

常用标签：

```text
haoran37/hnieoj-go-judge:latest
haoran37/hnieoj-go-judge:sha-<commit>
```

项目采用简单 Git Flow：日常开发进入 `develop`，合并到 `master` 后才触发 GitHub Actions 构建并发布 Docker Hub 镜像。生产部署建议使用 `sha-<commit>` 固定标签，避免 `latest` 漂移。

## 节点凭证

### formal 正式节点

1. 管理员调用 `POST /api/admin/judge/nodes/formal-tokens`，body `{nodeName,maxConcurrency,supportedJudgeModes}`。
2. 后端返回独立 `{token,tokenType,nodeId,tokenId,expireTime}`。
3. 运维把返回的 `data` JSON 保存到节点凭证文件（默认 `/etc/hnieoj/judge-node/credential.json`，权限 0600），或写入配置 `hnieoj.credential.token`。
4. 节点续期成功后会用 0600 权限原子替换凭证文件，重启优先读取最新续期凭证。凭证文件只保存运行凭证，不含任何全局主密钥。

### temp 临时节点

1. 管理员签发授权码。
2. 节点首次启动用 `authCode` 调用 `POST /api/judge/temp-token` 注册，并把凭证原子写入凭证文件。
3. 之后只调用 `POST /judge/nodes/token/renew` 续期，保持 `nodeId/tokenId`，不会再次兑换授权码；临时授权最晚期限由后端 `authorizationUntil` 控制。

节点不会把凭证写入日志。

## Ubuntu 部署

当前部署脚本只适配 Ubuntu，并会检查 Docker、Docker Compose v2 插件和 Docker daemon 状态。默认部署目录如下：

```text
/etc/hnieoj/go-judge
/etc/hnieoj/judge-node
/data/oj/judge-cache
```

一键部署最新镜像：

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/master/deploy/deploy-judge-node.sh -o /tmp/hnieoj-judge-node.sh && sudo IMAGE_TAG=latest bash /tmp/hnieoj-judge-node.sh deploy
```

指定固定镜像版本：

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/master/deploy/deploy-judge-node.sh -o /tmp/hnieoj-judge-node.sh && sudo IMAGE_TAG=sha-xxxxxxx bash /tmp/hnieoj-judge-node.sh deploy
```

首次执行 `deploy` 时，脚本会交互式生成配置文件和 Compose 文件、拉取镜像并重建容器。formal 节点需要提前准备运行凭证：

```text
/etc/hnieoj/judge-node/credential.json
```

temp 节点只需在初始化时输入授权码，首次启动后凭证会自动落到上述文件。

常用命令：

```bash
sudo bash /tmp/hnieoj-judge-node.sh doctor
sudo bash /tmp/hnieoj-judge-node.sh ps
sudo bash /tmp/hnieoj-judge-node.sh logs hnieoj-judge-node
sudo bash /tmp/hnieoj-judge-node.sh down
```

## 生产注意事项

- 不要将 go-judge 沙箱 HTTP 端口暴露到公网。
- 后端网关必须使用 HTTPS；不要为非回环地址配置明文 HTTP。
- 凭证目录需要可写，以便续期后原子替换凭证文件。
- 保持 `-file-timeout` 开启，避免沙箱临时文件长期堆积。
- 心跳间隔建议保持 30 秒左右，不要调到 1 秒级。
- SPJ 和交互题需要后端、题目数据和判题节点完整联调后再开启。
- Docker Hub 扫描中的 Debian CVE 依赖上游安全包修复；镜像构建会执行系统包升级，但未发布修复包的漏洞仍可能显示。

## 本地开发

```bash
go test ./internal/hnieoj/... ./cmd/hnieoj-judge-node
go test -race ./internal/hnieoj/... ./cmd/hnieoj-judge-node
go vet ./internal/hnieoj/... ./cmd/hnieoj-judge-node
go build -o ./tmp/go-judge ./cmd/go-judge
go build -o ./tmp/hnieoj-judge-node ./cmd/hnieoj-judge-node
```

连接本地后端调试时，`hnieoj.baseUrl` 可填写 `http://127.0.0.1:8800`；远程必须 HTTPS。

`-fixture` 模式仅用于本地/测试排查，且强制使用日志上报（`reporter.mode: log`），不会把任意 fixture 结果写入真实后端。

本地构建镜像：

```bash
docker build -f Dockerfile.hnieoj -t haoran37/hnieoj-go-judge:dev .
```
