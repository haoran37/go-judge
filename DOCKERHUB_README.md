# HnieOJ go-judge 判题机镜像

这是 HnieOJ 判题机镜像，基于 [criyle/go-judge](https://github.com/criyle/go-judge) 二次开发，包含：

- `go-judge` 沙箱服务；
- HnieOJ 判题节点 Agent（统一 Ed25519 身份 + WSS 任务通道）；
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
  --cgroupns=host \
  --shm-size=512m \
  -e HNIEOJ_WEB_ADDR=0.0.0.0:3723 \
  -p 127.0.0.1:3723:3723 \
  -v hnieoj-judge-state:/var/lib/hnieoj-judge-node \
  -v hnieoj-judge-cache:/data/oj/judge-cache \
  haoran37/hnieoj-go-judge:latest
```

`--cgroupns=host` 必需：upstream 沙箱在 Docker 默认 private cgroup namespace 下可能报 `cgroup path empty`；容器需加入宿主机 cgroup 命名空间（需在支持 cgroup v2 的 Linux + Docker 上验证）。

通过 `http://127.0.0.1:3723` 访问（建议 SSH 隧道，不要直接暴露公网）。首次进入 WebUI 创建管理员密码，再填写后端地址与一次性 Bootstrap 完成 Ed25519 入网。

## 说明

- 容器内 WebUI 通过 `HNIEOJ_WEB_ADDR=0.0.0.0:3723` 监听以配合端口映射；宿主机默认只映射到 loopback。程序默认绑定 `127.0.0.1:3723`。
- 后端必须 HTTPS/WSS；仅回环地址允许明文 HTTP/WS 用于本地开发，不提供跳过证书校验的开关。
- formal 与 temp 两类节点使用同一套身份机制：本地生成随机 Ed25519 keypair 与 `enrollmentId`，用一次性 Bootstrap 完成挑战注册；Bootstrap 在注册成功后删除。
- 注册回复丢失时用同一 `enrollmentId` + 公钥重试可恢复同一 node；重启复用身份，不消耗新 Bootstrap。
- 身份文件与结果持久队列位于状态目录 `/var/lib/hnieoj-judge-node`（0700），必须持久化；私钥绝不进入沙箱挂载或 WebUI 响应。
- 短期 AccessToken 过期后凭有效密钥重新认证（`AUTH_REFRESH`），不重新注册。
- 缓存目录 `/data/oj/judge-cache` 保存测试数据缓存，建议持久化。
- 不要把 go-judge 沙箱端口暴露到公网。
