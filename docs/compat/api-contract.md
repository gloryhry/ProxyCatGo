# ProxyCat API 兼容契约（M0-M4）

基线来源：`app.py`、`modules/modules.py`、前端 `web/templates/index.html`

## 鉴权规则
- 鉴权方式：query token（`?token=...`）
- 当 `[Server].token` 为空时，放行所有请求
- 需鉴权接口：
  - `GET /web`
  - `GET /api/status`
  - `GET /api/switch_proxy`
  - `POST /api/service`
  - `GET/POST /api/users`

## 控制面 API

### 1) `GET /api/status`
返回字段：
- `current_proxy` string
- `mode` string
- `port` int
- `interval` int
- `time_left` number
- `total_proxies` int
- `use_getip` bool
- `getip_url` string
- `auth_required` bool
- `display_level` int
- `service_status` (`running|stopped`)
- `config` object

### 2) `POST /api/config`
- 请求：JSON 键值对（保存到 `[Server]`）
- 响应：`status`、`port_changed`、`service_status`

### 3) `GET/POST /api/proxies`
- GET：`{proxies: string[]}`
- POST 请求：`{proxies: string[]}`
- POST 响应：`{status,message}`

### 4) `GET /api/check_proxies`
- 查询参数：`test_url`（可选，默认 `https://www.baidu.com`）
- 返回：`status`、`valid_proxies`、`total`、`message`
- 错误返回：`status=error` + `proxy_check_failed`
- 检测范围：`http|https|socks5`
- 检测缓存：按 `proxy + test_url`，TTL 10s

### 5) `GET/POST /api/ip_lists`
- GET：`{whitelist:string[],blacklist:string[]}`
- POST 请求：`{type:"whitelist|blacklist",list:string[]}`
- POST 响应：`{status,message}`

### 6) `GET /api/logs`
- 参数：`start`、`limit`、`level`、`search`
- 正常：`{status:"success",logs,total}`
- 参数非法：`{status:"error",message:<parse error>}`

### 7) `POST /api/logs/clear`
- 清空内存日志 + 文件日志
- 返回：`{status,message}`

### 8) `GET /api/switch_proxy`
- 成功：`{status:"success",current_proxy,message}`
- 冷却或不可切换：`{status:"error",message}`

### 9) `POST /api/service`
- 请求：`{action:"start|stop|restart"}`
- 返回：`{status,message,service_status}`
- 已联动真实数据面启停

### 10) `POST /api/language`
- 请求：`{language:"cn|en"}`
- 返回：`{status,language}`
- 非法语言：`unsupported_language`

### 11) `GET /api/version`
- 远程拉取：`https://y.shironekosan.cn/1.html`
- 正则提取：`<p>(ProxyCat-Vx.y.z)</p>`
- 返回：`status,is_latest,current_version,latest_version`
- 错误：`version_info_not_found` 或 `update_check_error`

### 12) `GET/POST /api/users`
- GET：`{users:{username:password}}`
- POST：`{users:{username:password}}`（回写 `[Users]`）

## 数据面协议（M2-M3）

### 入站协议识别
- 首字节 `0x05` -> SOCKS5
- 其他 -> HTTP 代理

### SOCKS5
- 方法协商：`0x00`、`0x02`
- `[Users]` 非空时强制用户名密码认证
- 支持 `CONNECT`
- 支持 IPv4 / DOMAIN / IPv6

### HTTP
- 支持普通 HTTP 转发
- 支持 `CONNECT` 隧道
- 认证失败返回 `407 Proxy Authentication Required`

### 上游代理桥接
- 支持 `socks5://`、`http://`、`https://`
- 支持上游认证
- `loadbalance`：按请求轮换
- `cycle`：按 interval 轮换

### M3 切换策略
- 手动切换冷却：默认 5s
- 失败切换阈值：默认连续 3 次
- 失败切换冷却：默认 3s
- `use_getip` 动态拉取 + 失败回退旧列表

## M5 稳定性增强（已落地）

### 数据面连接与清理
- 最大并发连接上限：`1000`（超额连接在 accept 后直接关闭）
- 单连接 I/O 超时：`120s`（入站连接统一 deadline）
- 活跃连接追踪：维护 `activeConns`，服务停止时统一关闭，避免悬挂连接
- 周期清理任务：每 `30s` 扫描失败计数缓存

### 失败恢复与缓存治理
- 连续失败计数：按上游代理地址累加
- 失败切换阈值：连续 `3` 次
- 失败切换冷却：`3s`
- 手动/自动切换冷却：`5s`
- 失败缓存保留：`5min`（超时自动清理，避免无界增长）
- `use_getip` 最小刷新间隔：`2s`（避免高频请求）

### 控制面可观测性
- `GET /api/status` 新增 `m5` 观测块：
  - `active_connections`
  - `max_concurrent_connections`
  - `failure_entries`
  - `failure_threshold`
  - `failure_cooldown_seconds`
  - `failure_retention_seconds`
  - `cleanup_interval_seconds`
  - `connection_timeout_seconds`

## M6 部署交付（已落地）

### 容器化文件
- `deploy/docker/Dockerfile`
  - 多阶段构建（`golang:1.25-alpine` -> `alpine:3.20`）
  - 构建产物：`/app/proxycat`
  - 携带运行时资源：`/app/web`、`/app/config`
  - 暴露端口：`1080`（代理）/ `5000`（控制面）
- `deploy/docker/docker-compose.yml`
  - 服务名：`proxycatgo`
  - 端口映射：`1080:1080`、`5000:5000`
  - 配置与日志挂载：`config -> /app/config`、`logs -> /app/logs`
  - 重启策略：`unless-stopped`

### 验证结果
- `go build ./...`：通过
- `docker compose -f deploy/docker/docker-compose.yml config`：通过
- `docker build -f deploy/docker/Dockerfile -t proxycatgo:test .`：通过
- 运行时验证（替代端口 `15000/18080`）：
  - `GET /api/status?token=honmashironeko` 返回 `service_status=running`
  - 返回包含 `m5` 观测字段，说明 M5/M6 联动正常

## 当前状态
- M0/M1/M2/M3/M4/M5/M6 主路径已打通
- 后续建议：补齐 README 迁移/回滚手册与自动化回归测试（非阻塞）