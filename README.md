# HnieOJ go-judge 判题机

HnieOJ go-judge 是基于 [criyle/go-judge](https://github.com/criyle/go-judge) 的二次开发版本，保留上游沙箱执行能力，并新增 HnieOJ 判题节点、WebUI 管理控制台、节点认证、任务消费、测试数据缓存、心跳和判题事件上报。

相关项目：

- 源仓库：[criyle/go-judge](https://github.com/criyle/go-judge)
- HnieOJ 后端：[haoran37/HnieOJ-backend](https://github.com/haoran37/HnieOJ-backend)
- HnieOJ 前端：[haoran37/HnieOJ](https://github.com/haoran37/HnieOJ)
- API 文档：[Apifox](https://s.apifox.cn/91edc2c6-6918-4179-9852-9ec3742377c8)

## 核心能力

- WebUI 控制台：镜像启动后访问 `3723` 端口，在浏览器完成初始化、配置、启动、停止、重启和状态查看。
- 正式节点：支持在 WebUI 上传 RSA 私钥，并通过后端/Nacos 获取 formal token。
- 临时节点：支持在 WebUI 输入授权码，系统自动生成本机实例密钥并兑换 JWT。
- 动态配置：配置保存到容器持久化目录，修改后可通过 WebUI 重启判题服务生效。
- 判题执行：内置 `go-judge` 沙箱服务，支持 C、C++17、Java 17、Python 3。
- 判题模式：支持 `default`、`spj`、`interactive`，启用前需确认后端和题目数据协议已闭环。
- 运维数据：展示运行状态、运行中任务、任务统计、最近错误、最近日志和缓存/磁盘相关信息。

## Docker 部署

默认镜像：

```text
haoran37/hnieoj-go-judge:latest
```

直接启动：

```bash
docker run -d \
  --name hnieoj-judge-node \
  --restart unless-stopped \
  --privileged \
  --shm-size=512m \
  -p 3723:3723 \
  -v hnieoj-judge-state:/var/lib/hnieoj-judge-node \
  -v hnieoj-judge-cache:/data/oj/judge-cache \
  haoran37/hnieoj-go-judge:latest
```

启动后访问：

```text
http://服务器IP:3723
```

首次进入 WebUI 后创建管理员密码，然后选择正式节点或临时节点完成初始化。

## 一键脚本

脚本只负责拉取镜像、替换旧容器、挂载状态目录和缓存目录、映射 WebUI 端口。私钥上传、临时令牌兑换、实例密钥生成和判题配置都在 WebUI 中完成。

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/master/deploy/deploy-judge-node.sh -o /tmp/hnieoj-judge-node.sh && sudo bash /tmp/hnieoj-judge-node.sh deploy
```

指定镜像 tag：

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/master/deploy/deploy-judge-node.sh -o /tmp/hnieoj-judge-node.sh && sudo IMAGE_TAG=sha-xxxxxxx bash /tmp/hnieoj-judge-node.sh deploy
```

修改宿主机访问端口只需要改 Docker 映射：

```bash
sudo WEBUI_HOST_PORT=8080 bash /tmp/hnieoj-judge-node.sh deploy
```

容器内 WebUI 固定监听 `3723`，不需要给程序额外传端口参数。

## 注意事项

- 不要把 go-judge 沙箱 HTTP 端口暴露到公网；WebUI 会在容器内管理沙箱进程。
- WebUI 可上传私钥和修改 MQ 密码，生产环境应限制访问来源，并使用强管理员密码。
- 状态目录 `/var/lib/hnieoj-judge-node` 必须持久化，否则管理员密码、配置和临时节点实例密钥会丢失。
- 缓存目录 `/data/oj/judge-cache` 建议持久化，避免大测试数据反复下载。
- 心跳间隔建议保持 30 秒左右，不要调到 1 秒级别。

## 本地开发

```bash
go test ./internal/hnieoj/... ./cmd/hnieoj-judge-node
go build -o ./tmp/hnieoj-judge-node ./cmd/hnieoj-judge-node
docker build -f Dockerfile.hnieoj -t haoran37/hnieoj-go-judge:dev .
```
