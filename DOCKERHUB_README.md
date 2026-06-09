# HnieOJ go-judge 判题机镜像

这是 HnieOJ 判题机镜像，基于 [criyle/go-judge](https://github.com/criyle/go-judge) 二次开发，包含：

- `go-judge` 沙箱服务；
- HnieOJ 判题节点适配层；
- WebUI 管理控制台；
- C、C++17、Java 17、Python 3 判题工具链。

相关项目：

- HnieOJ 后端：[haoran37/HnieOJ-backend](https://github.com/haoran37/HnieOJ-backend)
- HnieOJ 前端：[haoran37/HnieOJ](https://github.com/haoran37/HnieOJ)
- API 文档：[Apifox](https://s.apifox.cn/91edc2c6-6918-4179-9852-9ec3742377c8)

## 启动

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

访问：

```text
http://服务器IP:3723
```

首次进入 WebUI 后创建管理员密码，并在页面中选择正式节点或临时节点完成初始化。

## 说明

- 容器内 WebUI 固定监听 `3723`，宿主机端口通过 Docker `-p 宿主端口:3723` 映射。
- 状态目录 `/var/lib/hnieoj-judge-node` 保存管理员密码、判题配置、formal 私钥和 temp 实例密钥，必须持久化。
- 缓存目录 `/data/oj/judge-cache` 保存测试数据缓存，建议持久化。
- 不要把 go-judge 沙箱端口暴露到公网。
