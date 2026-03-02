# ProxyCat 配置兼容契约（M0）

基线来源：`config/config.ini` + `modules/modules.py:DEFAULT_CONFIG` + `app.py`

## 主配置文件
- 路径：`config/config.ini`
- 节：`[Server]`, `[Users]`

## [Server] 关键键
- `display_level`（默认 `1`）
- `port`（默认 `1080`）
- `web_port`（默认 `5000`）
- `mode`（`cycle|loadbalance`，默认 `cycle`）
- `interval`（默认 `300`）
- `use_getip`（`true|false`）
- `getip_url`
- `proxy_username` / `proxy_password`
- `proxy_file`（默认 `ip.txt`）
- `check_proxies`（`true|false`）
- `test_url`
- `health_check_enabled`（`true|false`，默认 `true`）
- `health_check_interval`（秒，默认 `300`）
- `health_check_timeout`（秒，默认 `8`）
- `health_check_concurrency`（默认 `50`）
- `health_check_auto_apply`（`true|false`，默认 `false`）
- `health_check_auto_persist`（`true|false`，默认 `false`）
- `health_check_min_pool_size`（默认 `1`）
- `health_check_mode`（`basic|traffic_simulation`，默认 `traffic_simulation`）
- `health_check_success_ratio`（`0~1`，默认 `0.67`）
- `health_check_attempts_per_proxy`（默认 `3`）
- `health_check_target_port`（默认 `443`）
- `language`（`cn|en`）
- `whitelist_file`（默认 `whitelist.txt`）
- `blacklist_file`（默认 `blacklist.txt`）
- `ip_auth_priority`（`whitelist|blacklist`）
- `token`
- `username` / `password`（保留）

## [Users]
- 语义：代理入站认证用户表，`username=password`
- 空 Users => `auth_required=false`

## 文件路径语义
- `proxy_file`/`whitelist_file`/`blacklist_file` 采用 basename，实际读取 `config/` 下同名文件。

## 热更新语义（当前阶段）
- `/api/config` 直接回写 `[Server]`
- `/api/users` 直接重建 `[Users]`
- 运行态通过内存配置刷新（最小化实现）
