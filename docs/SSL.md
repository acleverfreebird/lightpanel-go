# 一键 SSL（内置 ACME 客户端）

LightPanel 内置 Let's Encrypt ACME 客户端（`pkg/certs`，基于 `golang.org/x/crypto/acme`），在「网站 → 站点设置 → SSL 证书」中一键申请证书。**不依赖 certbot、acme.sh 或任何外部工具**，也不需要 Python/摘件脚本——签发、部署与续期全部由面板二进制自身完成。

## 使用条件

- 站点由面板创建（配置带 `# managed by lightpanel` 标记），引擎为 Nginx 或 Apache；Docker 容器站点与手工配置不支持改写（安全边界：面板永不改写不带标记的配置）。
- 站点绑定了具体域名（`server_name` 为 `_` 或通配符时无法签发；通配符需要 DNS-01，暂不支持）。
- 域名解析到本服务器，且 80 端口可从公网访问（HTTP-01 验证要求）。
- helper 已配置 `allow_sites = true`（最小特权模式；root 模式直接执行）。

## 签发流程（后台任务，任务中心可见每一步）

1. **准备挑战配置**：helper 重写站点配置，加入固定的 ACME 挑战 location（Nginx `location ^~ /.well-known/acme-challenge/` / Apache `Alias`，指向 `/var/lib/lightpanel/acme-challenges/`）并重载引擎。站点已启用 SSL（续期场景）时跳过此步——此前生成的配置已携带挑战路径。
2. **ACME 订单**：面板以存储的账号密钥向 Let's Encrypt 发起订单（可选邮箱注册；可勾选 Staging 测试证书，不受生产频控）。
3. **HTTP-01 验证**：挑战文件经 helper `challenge-set` 写入挑战目录（root 所有，目录 0755、文件 0644，nginx/apache worker 可读），验证完成后清理。
4. **保存证书**：`fullchain.pem`/`privkey.pem`/`meta.json` 写入证书存储目录。
5. **启用 HTTPS**：helper `ssl-apply` 重写站点配置（80 端口块 + 443 SSL 服务器块 + 证书路径，可选 80→443 强制跳转；跳转规则不覆盖挑战路径）并重载引擎。

任何一步失败：挑战文件被清理；首次签发场景会把站点配置回滚为签发前的纯 HTTP 版本。

安全护栏：helper 在重载前执行 `nginx -t` / `apachectl configtest`，配置测试失败自动恢复上一版配置，保证引擎永不因面板写入的坏配置而无法重载。

## 存储布局

```text
/var/lib/lightpanel/acme/            面板可写的 ACME 状态目录（install.sh 创建，0750）
├── account.key                      ACME 账号密钥（ECDSA P-256，0600）
└── certs/<域名>/
    ├── fullchain.pem                叶子证书 + 中间链（0644，nginx root 主进程读取）
    ├── privkey.pem                  站点私钥（0600）
    └── meta.json                    签发参数（邮箱、staging、强制 HTTPS、站点配置快照）

/var/lib/lightpanel/acme-challenges/ HTTP-01 挑战文件（helper root 所有，0755/0644）
```

`/var/lib/lightpanel/acme` 不可写时（旧版安装未预建目录），面板回退到用户状态目录 `<home>/.local/state/lightpanel/acme`。

## 续期

面板进程内置续期循环：启动 2 分钟后及之后每 6 小时扫描证书库，到期剩余不足 30 天（与 certbot 默认一致）自动重签并重载引擎。站点已关闭 SSL（配置中不再有 443/`SSLEngine`）时跳过续期；Staging 测试证书不自动续期。

## API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/sites/certs` | 证书列表（域名、颁发者、有效期、剩余天数、staging、签发站点参数） |
| POST | `/api/sites/ssl` | `op=issue,id,email?,force_https?,staging?` 签发；`op=off,id` 关闭 SSL |

签发是异步任务：响应携带 `task_id`，前端自动打开任务中心展示进度（与站点创建、应用安装共用同一套任务中心）。

## 已知限制

- 通配符域名需要 DNS-01 与 DNS 服务商 API，暂不支持。
- Docker 容器站点与手工创建的配置不参与一键 SSL（可为其前端的 Nginx/Apache 反代站点签发）。
- HTTP-01 要求 80 端口公网可达；若面板所在的 80 端口由其他服务占用且无法改写其配置，签发会失败（CA 返回的验证错误会原样显示在任务日志里）。
