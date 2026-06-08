# HnieOJ go-judge 判题机

HnieOJ go-judge 是基于 [criyle/go-judge](https://github.com/criyle/go-judge) 的二次开发版本。它保留上游沙箱执行能力，并新增 HnieOJ 判题节点适配层，用于连接后端任务队列、节点认证、测试数据缓存、心跳和判题事件上报。

这个仓库面向 HnieOJ 生产部署，不是上游 go-judge 的通用替代发行版。

## 相关项目

- 源仓库：[criyle/go-judge](https://github.com/criyle/go-judge)
- HnieOJ 后端：[HnieOJ 后端仓库](https://github.com/haoran37/HNieOJ-backend)
- HnieOJ 前端：[HnieOJ 前端仓库](https://github.com/haoran37/HNieOJ)
- API 文档：[Apifox](https://s.apifox.cn/91edc2c6-6918-4179-9852-9ec3742377c8)

## 核心能力

- 沙箱执行：通过 `go-judge` 在受限环境中编译、运行和采集程序结果。
- 判题节点：通过 `hnieoj-judge-node` 消费 RabbitMQ 判题任务，并向 HnieOJ 后端上报事件。
- 节点认证：支持 `formal` 长期节点和 `temp` 临时节点。
- 临时节点预认证：部署脚本会在容器启动前用授权码兑换 JWT，失败时立即要求重新输入。
- 测试数据缓存：支持缓存容量、未使用时间、定时清理和心跳统计采样。
- 节点心跳：上报节点在线状态、并发能力、磁盘/缓存统计和支持的判题模式。
- 判题模式：支持 `default`、`spj`、`interactive`，生产默认只开启 `default`。
- 语言环境：Docker 镜像内置 C、C++17、Java 17、Python 3 工具链。

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

## Linux 部署

部署脚本面向主流 Linux 发行版，已在 Ubuntu 系列环境验证；其他发行版只要求具备 Bash、Docker Engine、Docker Compose v2 插件，并能访问 Docker daemon。默认部署目录如下：

```text
/etc/hnieoj/go-judge
/etc/hnieoj/judge-security
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

首次执行 `deploy` 时，脚本会交互式生成配置文件、渲染 Compose 文件、拉取镜像并重建容器。临时节点会先兑换 JWT；正式节点需要提前准备私钥：

```text
/etc/hnieoj/judge-security/judge_formal_private.pem
```

常用命令：

```bash
sudo bash /tmp/hnieoj-judge-node.sh doctor
sudo bash /tmp/hnieoj-judge-node.sh ps
sudo bash /tmp/hnieoj-judge-node.sh logs hnieoj-judge-node
sudo bash /tmp/hnieoj-judge-node.sh down
```

## 生产注意事项

- 不要将 go-judge 沙箱 HTTP 端口暴露到公网。
- 保持 `-file-timeout` 开启，避免沙箱临时文件长期堆积。
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
