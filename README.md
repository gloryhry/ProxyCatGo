# ProxyCatGo

<p align="center">
  ProxyCat 的 Go 重构版本（高兼容迁移）
</p>

---

## 目录

- [项目简介](#项目简介)
- [功能特点](#功能特点)
- [快速开始](#快速开始)
- [配置说明](#配置说明)
- [控制面 API 概览](#控制面-api-概览)
- [Docker 部署](#docker-部署)
- [灰度迁移与回滚](#灰度迁移与回滚)
- [故障排查](#故障排查)
- [免责声明](#免责声明)
- [License](#license)

## 项目简介

ProxyCatGo 是对原 Python 版 ProxyCat 的 Go 重构实现，目标是：

- 控制面 API 尽量保持 1:1 兼容（路径、字段、鉴权语义）
- 数据面协议行为保持一致（HTTP/SOCKS5 入站 + 上游桥接）
- 配置与国际化语义兼容（`config.ini`、`[Users]`、中英消息键）

当前里程碑状态：**M0-M6 主路径已打通**。

兼容契约文档：
- `docs/compat/config-contract.md`
- `docs/compat/api-contract.md`

## 功能特点

- 支持 HTTP / SOCKS5 入站代理
- 支持上游 `http://` / `https://` / `socks5://` 代理桥接
- 支持上游认证与入站认证（`[Users]`）
- 支持 `cycle` / `loadbalance` 切换策略
- 支持 `use_getip` 动态拉取代理 + 失败回退
- 支持 Web 控制面 + REST API 管理
- 支持 query token 鉴权（`?token=...`）
- 支持中英双语消息
- 支持 Docker / GHCR 镜像部署

## 快速开始

### 环境要求

- Go >= 1.25
- Linux / macOS / Windows

### 本地构建

```bash
go build ./...
```

### 本地启动

```bash
go run ./cmd/proxycat
```

默认端口：
- 数据面：`1080`
- 控制面：`5000`

## 配置说明

主配置文件：`config/config.ini`

关键配置项（`[Server]`）：

- `port`：代理监听端口（默认 `1080`）
- `web_port`：控制面端口（默认 `5000`）
- `mode`：`cycle|loadbalance`
- `interval`：切换间隔（秒）
- `use_getip`：是否启用动态拉取
- `getip_url`：动态拉取地址
- `proxy_file`：代理文件名（默认 `ip.txt`）
- `token`：控制面 token（为空则不鉴权）
- `language`：`cn|en`

用户认证（`[Users]`）：

```ini
[Users]
user1 = pass1
user2 = pass2
```

当 `[Users]` 非空时，数据面会启用用户名密码认证。

## 控制面 API 概览

常用接口：

- `GET /api/status`
- `POST /api/config`
- `GET/POST /api/proxies`
- `GET /api/check_proxies`
- `GET/POST /api/ip_lists`
- `GET /api/logs`
- `POST /api/logs/clear`
- `GET /api/switch_proxy`
- `POST /api/service`
- `POST /api/language`
- `GET /api/version`
- `GET/POST /api/users`

鉴权示例：

```bash
curl "http://127.0.0.1:5000/api/status?token=honmashironeko"
```

完整契约以 `docs/compat/api-contract.md` 为准。

## Docker 部署

### 方式一：直接拉取 GHCR 镜像

```bash
docker pull ghcr.io/gloryhry/proxycatgo:latest
```

```bash
docker run -d \
  --name proxycatgo \
  -p 1080:1080 \
  -p 5000:5000 \
  -v $(pwd)/config:/app/config \
  -v $(pwd)/logs:/app/logs \
  --restart unless-stopped \
  ghcr.io/gloryhry/proxycatgo:latest
```

### 方式二：使用 Compose

```bash
docker compose -f deploy/docker/docker-compose.yml up -d
```

停止：

```bash
docker compose -f deploy/docker/docker-compose.yml down
```

### 本地构建镜像（开发调试）

```bash
docker build -f deploy/docker/Dockerfile -t proxycatgo:local .
```

## 灰度迁移与回滚

### 灰度建议

1. 先使用备用端口启动 Go 版本（例如 `15000->5000`, `18080->1080`）。
2. 验证控制面关键链路（状态、配置保存、服务启停、代理切换）。
3. 验证数据面链路（HTTP/SOCKS5、上游认证、切换稳定性）。
4. 逐步切流到 Go 版本。

### 回滚路径

如果出现异常：

1. 停止 Go 进程/容器。
2. 切回 Python 版本启动（原仓库根目录）：

```bash
python app.py
```

3. 复用相同 `config/` 与代理文件即可恢复原链路。

## 故障排查

- `api/status` 返回 `invalid_token`
  - 检查 URL 是否带 `?token=...`
  - 检查 `config/config.ini` 的 `token` 配置

- Docker 启动失败提示端口占用
  - 检查主机 `1080/5000` 是否被占用
  - 临时改成映射到其它端口验证

- 上游连接失败
  - 核对 `ip.txt` 代理格式（`scheme://[user:pass@]host:port`）
  - 检查目标网络连通性

## 免责声明

- 本项目仅用于授权测试、研究和合法合规场景。
- 请勿将本项目用于任何未授权或非法用途。
- 使用本项目造成的风险与后果由使用者自行承担。

## License

本项目沿用原项目许可证：`GPL-2.0`。

详见：`LICENSE`
