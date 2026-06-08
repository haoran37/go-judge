# HNieOJ go-judge 判题机

本仓库是在 [criyle/go-judge](https://github.com/criyle/go-judge) 基础上的 HNieOJ 二次开发版本，保留 go-judge 沙箱能力，并增加 HNieOJ 判题节点适配层，用于接入后端任务队列、测试数据缓存、节点认证、心跳和判题事件上报。

相关项目：

- 源仓库：[https://github.com/criyle/go-judge](https://github.com/criyle/go-judge)
- HNieOJ 后端：[https://github.com/haoran37/HNieOJ-backend](https://github.com/haoran37/HNieOJ-backend)
- HNieOJ 前端：[https://github.com/haoran37/HNieOJ](https://github.com/haoran37/HNieOJ)
- API 文档：[https://s.apifox.cn/91edc2c6-6918-4179-9852-9ec3742377c8](https://s.apifox.cn/91edc2c6-6918-4179-9852-9ec3742377c8)

## 功能

- go-judge 沙箱服务：提供受限程序执行环境。
- HNieOJ 判题节点：消费 RabbitMQ 判题任务，调用沙箱执行，向后端上报判题事件。
- 节点认证：支持 formal 长期节点和 temp 临时节点。
- 临时节点预认证：部署脚本会先用临时授权码兑换 JWT，成功后再启动容器。
- 测试数据缓存：支持容量、未使用时间和定时清理。
- 节点心跳：上报节点状态、并发能力、缓存统计和支持的判题模式。
- 判题模式：支持 `default`、`spj`、`interactive`，默认只开启 `default`。
- 运行环境：Docker 镜像内置 C、C++17、Java 17、Python 3 工具链。

## 镜像与分支

Docker Hub 镜像：

```text
haoran37/hnieoj-go-judge:latest
haoran37/hnieoj-go-judge:sha-<commit>
```

开发采用简单 Git Flow：日常修改进入 `develop`，合并到 `master` 后才触发 GitHub Actions 构建并发布 Docker Hub 镜像。生产部署建议优先使用 `sha-<commit>` 固定标签，避免 `latest` 漂移。

## Ubuntu 一句话部署

当前部署脚本只适配 Ubuntu。脚本会检查 Docker、Docker Compose v2 插件和 Docker daemon 状态；默认会写入 `/etc/hnieoj/go-judge`、`/etc/hnieoj/judge-security`、`/data/oj/judge-cache`，因此建议用 `sudo` 执行。

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/master/deploy/deploy-judge-node.sh -o /tmp/hnieoj-judge-node.sh && sudo IMAGE_TAG=latest bash /tmp/hnieoj-judge-node.sh deploy
```

指定固定镜像版本：

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/master/deploy/deploy-judge-node.sh -o /tmp/hnieoj-judge-node.sh && sudo IMAGE_TAG=sha-xxxxxxx bash /tmp/hnieoj-judge-node.sh deploy
```

## 部署脚本行为

`deploy` 命令会完成以下操作：

- 首次运行时交互式生成 `/etc/hnieoj/go-judge/config.yaml`。
- temp 节点会在容器启动前兑换 JWT，失败会要求重新输入授权码。
- 渲染 `/etc/hnieoj/go-judge/compose.yaml`。
- 拉取 `haoran37/hnieoj-go-judge:<tag>`。
- 检查配置、Docker Compose 和 formal 节点私钥。
- 停止并重建旧容器。

常用命令：

```bash
sudo bash /tmp/hnieoj-judge-node.sh deploy
sudo bash /tmp/hnieoj-judge-node.sh doctor
sudo bash /tmp/hnieoj-judge-node.sh ps
sudo bash /tmp/hnieoj-judge-node.sh logs hnieoj-judge-node
sudo bash /tmp/hnieoj-judge-node.sh down
```

formal 节点需要提前放置私钥：

```text
/etc/hnieoj/judge-security/judge_formal_private.pem
```

## 生产注意事项

- 不要把 go-judge 沙箱 HTTP 端口暴露到公网。
- 保持 `-file-timeout` 开启，避免沙箱文件缓存长期堆积。
- 心跳间隔建议保持 30 秒左右，不要调到 1 秒级。
- SPJ 和交互题需要后端、题目数据和判题节点完整联调后再开启。
- Docker Hub 扫描中的 Debian CVE 依赖上游安全包修复；镜像构建会执行系统包升级，但未发布修复包的漏洞仍可能显示。

## 本地开发

```bash
go test ./...
go build -o ./tmp/go-judge ./cmd/go-judge
go build -o ./tmp/hnieoj-judge-node ./cmd/hnieoj-judge-node
```

本地构建镜像：

```bash
docker build -f Dockerfile.hnieoj -t haoran37/hnieoj-go-judge:dev .
```
