# HnieOJ go-judge 判题机镜像

这是 HnieOJ 判题节点镜像，基于 [criyle/go-judge](https://github.com/criyle/go-judge) 二次开发，包含：

- `go-judge` 沙箱服务；
- `hnieoj-judge-node` 判题节点适配层；
- C、C++17、Java 17、Python 3 判题工具链。

相关项目：

- HnieOJ 后端：[HnieOJ 后端仓库](https://github.com/haoran37/HNieOJ-backend)
- HnieOJ 前端：[HnieOJ 前端仓库](https://github.com/haoran37/HNieOJ)
- API 文档：[https://s.apifox.cn/91edc2c6-6918-4179-9852-9ec3742377c8](https://s.apifox.cn/91edc2c6-6918-4179-9852-9ec3742377c8)

## 镜像标签

```text
haoran37/hnieoj-go-judge:latest
haoran37/hnieoj-go-judge:sha-<commit>
```

`latest` 来自 `master` 分支最新构建；生产环境建议使用 `sha-<commit>` 固定标签。

## Linux 一键部署

部署脚本面向主流 Linux 发行版，已在 Ubuntu 系列环境验证；其他发行版只要求具备 Bash、Docker Engine、Docker Compose v2 插件，并能访问 Docker daemon。

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/master/deploy/deploy-judge-node.sh -o /tmp/hnieoj-judge-node.sh && sudo IMAGE_TAG=latest bash /tmp/hnieoj-judge-node.sh deploy
```

指定固定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/haoran37/go-judge/master/deploy/deploy-judge-node.sh -o /tmp/hnieoj-judge-node.sh && sudo IMAGE_TAG=sha-xxxxxxx bash /tmp/hnieoj-judge-node.sh deploy
```

## 部署脚本做什么

- 交互式生成 `/etc/hnieoj/go-judge/config.yaml`。
- temp 节点启动前先用授权码兑换 JWT，失败会立即要求重新输入。
- 渲染 `/etc/hnieoj/go-judge/compose.yaml`。
- 拉取指定 Docker Hub 镜像。
- 校验配置和 Docker 环境。
- 重建旧容器。

## 注意事项

- 不要将沙箱 HTTP 端口暴露到公网。
- 保持 `-file-timeout` 开启，避免临时文件长期堆积。
- formal 节点需要挂载私钥到 `/etc/hnieoj/judge-security/judge_formal_private.pem`。
- SPJ 和交互题需要后端、题目数据和节点联调完成后再开启。
- 镜像构建会升级 Debian 系统包；若 Docker Hub 扫描仍显示 CVE，通常需要等待 Debian 发布修复包或后续切换基础镜像。
