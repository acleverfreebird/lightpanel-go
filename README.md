# LightPanel — 轻量 Linux 服务器管理面板

在现有代码基础上完成的可运行 MVP。运行时只需要一个 Linux 二进制；HTML、CSS、原生 JavaScript 均通过 `go:embed` 内嵌，没有 CDN、前端构建链、Node.js、Python、Docker 或数据库依赖。

## 1. 项目整体架构设计

### 技术选择

- Go **1.25+** + 标准库 `net/http` / `html/template` / `log/slog`。
- 原生 JS + 服务端页面模板 + 同源 JSON API；相比 HTMX + Alpine.js 少两个运行时资源，使用 CSP 禁止内联脚本。
- `os.Root` 钉住 `/` 根目录句柄，文件管理全程走 openat 而非字符串路径拼接，`..` 组件直接拒绝。Go 1.25 是项目最低编译版本。
- `bcrypt` 保存密码哈希；256 位随机 session 和 CSRF token；不需要 JWT 密钥、SQLite 或持久 session。重启使全部 session 失效。
- 六个直接模块依赖：`go-toml/v2`、`x/crypto`、`x/sys`、`x/term`、`gorilla/websocket`、`creack/pty`。`x/term` 用于隐藏输入密码；Web Terminal 使用 WebSocket 与 Linux PTY。`CGO_ENABLED=0` 可编译运行产物。
- Linux `/proc` / `statfs` 获取指标；systemd 和防火墙通过已安装的系统命令调用，没有 `sh -c`。

### 目录结构

```text
main.go                      启动、TLS、密码哈希 CLI、信号和优雅关闭
server.go                    嵌入资源、路由、响应安全头、并发限制、审计
diagnostics.go               已认证运行诊断（身份模式、systemd/工具、helper 可达性）
config/config.go             严格 TOML 解析、环境变量覆盖、配置校验
pkg/auth/auth.go             登录限流、session、权限和 CSRF
pkg/sysinfo/command.go       固定工具路径、超时和输出上限
pkg/sysinfo/metrics.go       CPU/内存/磁盘/网络/负载/运行时间
pkg/sysinfo/process.go       /proc 进程搜索、分页、pidfd 信号
pkg/sysinfo/service.go       systemd 列表/状态/启停/重启
pkg/sysinfo/filemanager.go   全盘文件浏览/上传/下载/新建/重命名/编辑/递归删除/chmod/批量复制/移动/回收站（面板入口，root 面板直连执行）
pkg/sysinfo/filemanager_remote.go 非 root 面板的文件操作转发（helper 中继：列表/读取 JSON 直传，下载/上传/保存流式，批量操作逐项聚合）
pkg/helper/files_linux.go    helper 端 root 文件操作执行体（路径复验、虚拟目录保护、流式中继与提交帧）
pkg/helper/fileops_linux.go  面板与 helper 共享的复制/移动/回收站执行体（不覆盖语义、跨设备回退、恢复元数据）
pkg/sysinfo/update.go        检查 GitHub Release、校验 SHA256、替换二进制并重启服务
pkg/sysinfo/logs.go          journalctl 系统与服务日志
pkg/sysinfo/firewall.go      UFW/firewalld 状态和端口规则
pkg/sysinfo/sites.go         站点管理：环境识别、nginx/Apache 配置解析、静态站点与 Docker 部署
pkg/sysinfo/certs.go         一键 SSL：内置 ACME 编排、签发任务与自动续期
pkg/certs/certs.go           Let's Encrypt ACME 客户端（golang.org/x/crypto/acme）与证书存储
pkg/sysinfo/apps.go          应用商店：应用目录状态检测、后台安装/卸载任务
pkg/sysinfo/databases.go     数据库管理：引擎识别、库与用户列表、一步建库、备份/恢复/下载、SQL 控制台
pkg/sysinfo/dbstore.go       数据库凭据备忘（面板创建的库/账号密码，0600 JSON 文件）
pkg/helper/protocol.go       最小特权 helper 协议（请求/响应、目录校验）
pkg/helper/sites.go          站点校验器/配置模板/托管目录规则（面板与 helper 共享）
pkg/helper/apps.go           应用目录与软件包管理器安装参数白名单（面板与 helper 共享）
pkg/helper/databases.go      数据库引擎/名称/密码校验与操作命令构建，备份文件名与 gzip 辅助（面板与 helper 共享）
pkg/helper/acl.go            按服务/动作的授权 ACL 与防火墙参数白名单
pkg/helper/client.go         面板侧 helper 客户端（unix socket）
pkg/helper/server_linux.go   helper 服务端：SO_PEERCRED、白名单执行、更新安装
pkg/**/*_test.go             安全边界与解析测试
server_test.go              路由、登录、权限、文件流程、审计测试
static/app.js, app.css       无第三方框架的宝塔式管理界面（深色侧栏、绿色主调、首页仪表盘）
templates/*.html            登录页与管理页模板
config/example.toml        无预置密码的配置样例与升级迁移模板
lightpanel.service          systemd 单元（非特权面板）
lightpanel-helper.service   systemd 单元（root helper）
Makefile                    Linux amd64/arm64 构建与测试
.github/workflows/release.yml  推送 v* 标签时构建发布产物（amd64/arm64 + SHA256SUMS）
.github/workflows/ci.yml       push/PR 时运行 vet、race 测试、双架构构建、JS 测试与本机 HTTP 冒烟
scripts/smoke.py             可选开发验证脚本（非运行依赖）
docs/VALIDATION.md           验证记录与已知限制
docs/SSL.md                  一键 SSL（内置 ACME）实现、存储布局与已知限制
```

### MVP 范围

已实现：系统概览（指标趋势、主机信息、运行诊断、快捷入口）、进程搜索/分页/结束、systemd 服务管理（已加载与已安装单元、启停/重启/重载/开机自启）、全盘文件浏览/上传/下载/新建文件夹/重命名/在线编辑/权限/多选批量操作（复制、剪切、粘贴、批量删除）/回收站（删除先进回收站，可恢复到原位置、逐条彻底删除或清空；存放于 /var/lib/lightpanel/trash；复制/移动/粘贴永不覆盖同名条目，同名冲突逐项报告）（非 root 面板经 helper 以 root 执行，宝塔式全盘文件管理）、版本检查与一键更新、单管理员登录和可选只读权限、系统/服务日志、防火墙端口规则、网站管理（宝塔式布局：站点表格 + 设置弹窗；自动识别 Nginx/Apache/Docker，浏览已配置站点，创建静态站点、反向代理或 Docker 容器部署，受控删除与重载；内置 ACME 客户端一键申请 Let's Encrypt 证书——不需要 certbot 等任何外部工具，支持强制 HTTPS、到期前 30 天自动续期与 Staging 测试证书）、应用商店（通过系统软件包管理器一键安装/卸载 Nginx/Apache/Docker/MySQL/MariaDB/PostgreSQL/Redis，安装与卸载均为后台任务并可查看进度与输出）、数据库管理（自动识别 MySQL/MariaDB/PostgreSQL/Redis；宝塔式数据库表格：搜索、字符集、容量、账号与密码查看/复制；一步建库=建库+建号+授权+字符集+访问来源选择，凭据备忘保存在面板状态目录；每库备份中心：后台备份/恢复/下载/删除备份，流式 gzip 存放于 /var/backups/lightpanel/databases；内置 SQL 控制台直接对库执行语句并表格化展示结果；创建用户与修改密码，引擎启停）。终端（Web Terminal）为宝塔风格的网页终端：侧栏打开即连，整页控制台（内置 xterm.js + 真 PTY，支持 vim/top、窗口自适应与 5000 行回滚），进入即 **root 登录 shell**（root 面板本地派生，非 root 面板经 helper 中继），复用面板登录会话，跨源握手拒绝、退出登录即时撤销并保留生命周期审计；详见 [终端](docs/WEB-TERMINAL.md)。

安全边界：这是有权限的主机管理工具，不是多租户容器。文件管理面向**整个文件系统**：所有接口只接受绝对路径，`..` 组件、反斜杠与 NUL 一律拒绝；`/proc`、`/sys`、`/dev`、`/run` 这四个虚拟系统目录拒绝删除、移动、复制与剪切，回收站内的条目也只能经回收站接口清除。下载/编辑读取只接受普通文件（符号链接若最终指向普通文件也可下载）；chmod 只接受普通文件与目录，且拒绝 setuid/setgid 与符号链接；只允许普通文件上传，禁止覆盖；目录删除默认要求为空，带 `recursive=true` 时递归删除且不允许删除根；在线编辑只处理 ≤1 MiB 且不含 NUL 的普通文件，保存先写临时文件再原子替换，且拒绝以符号链接为目标的写入。批量复制/移动/回收与恢复全部采用**不覆盖**语义（目标同名即拒绝，绝不静默替换），目录不会被复制/移动进其自身子树；回收站条目以随机 id 目录存放（`item` + `meta.json`，0700/0600），id 按字母表白名单校验、原始路径恢复前复验，跨文件系统的回收/移动/恢复先完整复制再删除源，任何中途失败都不会丢失原文件。非 root 面板的这些接口经 helper 以 root 权限执行（`allow_files`，见「最小特权 helper」），root 面板则直接执行；无论哪种模式，请务必启用 TLS/反代并保管好管理员密码。

## 2. 核心数据流与接口设计

```text
浏览器 → Host 白名单 / CSP / 请求体上限 / 并发上限
       → 随机 session 校验（8 小时过期）
       → 变更请求：写权限 + 同源 Origin/Referer + CSRF
       → 审计开始 → 模块处理器 → 审计结果（结构化 JSON）
       → /proc、目录句柄或白名单系统命令 → JSON / 下载流
```

API 默认必须登录。页面 `GET /` 未登录时跳转到 `/login`；API 返回 401。只有登录页和静态资源公开。`GET /` 使用模板转义账号；动态文本使用 `textContent`，上传 HTML 以附件下载并带 `nosniff`，不会在面板来源下执行。

所有变更为 POST。除退出表单外，必须通过 `X-CSRF-Token` 传入页面 meta 中的 token，并带与 `public_origin` 完全一致的 Origin（或同源 Referer）。不会信任 `X-Forwarded-For` 等外部转发头。反代应保留 Host；来自同一个代理的登录失败按代理 IP 限流。

| 方法 | 路径 | 参数 / 返回 |
|---|---|---|
| GET/POST | `/login` | 登录页 / `username,password` 表单 |
| POST | `/logout` | CSRF，立即撤销服务端 session |
| GET | `/api/metrics` | 主机、CPU%、内存/磁盘字节、负载、uptime 秒、网络 B/s、自身 RSS |
| GET | `/api/processes` | `q` 按进程名/PID 搜索，`page`；每页 100 条，按 RSS 排序 |
| POST | `/api/process/kill` | `pid,signal=15或9,start_time`；身份不符返回 409 |
| GET | `/api/services` | 不带 `name` 列出已加载与已安装（`list-unit-files` 合并）的服务；带 `.service` 名返回详情 |
| POST | `/api/service/action` | `name,action=start或stop或restart或reload或enable或disable`；enable/disable 仅改开机启动配置 |
| GET | `/api/files` | `path=/` 绝对路径，`offset=0`；每页最多 200 条，条目含 `modified`（Unix 秒）与 `symlink` 标记 |
| GET | `/api/file/download` | `path`；流式附件下载（root 面板模式支持 Range；helper 模式为整文件流式下载，Content-Length 校验完整性） |
| POST | `/api/file/upload?path=...` | 请求体是文件原始字节，非 multipart；上限 `max_upload_mb`（默认 32 MiB），禁止覆盖 |
| GET | `/api/file/read` | `path`；读取 ≤1 MiB 文本文件内容用于编辑，含 NUL 或超限返回 400 |
| POST | `/api/file/write?path=...` | 请求体是新内容（≤1 MiB）；先写临时文件再原子替换，拒绝符号链接目标 |
| POST | `/api/file/mkdir` | `path`；`MkdirAll` 语义，可一次创建多级目录（0750） |
| POST | `/api/file/rename` | `path,to`；均为绝对路径，可在改名的同时移动，`to` 不得为根 |
| POST | `/api/file/delete` | `path`；可选 `recursive=true` 递归删除，不允许删除根 |
| POST | `/api/file/chmod` | `path,mode`；普通文件与目录，三位八进制 000–777，不允许 setuid/setgid 与符号链接 |
| POST | `/api/file/copy` | `path`（可重复，≤200 项）`+to` 目标目录；批量复制到 `to` 下（保留原名），不覆盖同名，逐项返回 `done/failed` |
| POST | `/api/file/move` | `path`（可重复）`+to`；批量移动（同设备 rename，跨设备先复制后删源），不覆盖同名，目录不可移入自身子树 |
| POST | `/api/file/trash` | `path`（可重复）；批量移入回收站（`/var/lib/lightpanel/trash`，0700），可恢复 |
| GET | `/api/file/trash-list` | 回收站列表：id、名称、原位置、大小、删除时间，按删除时间倒序 |
| POST | `/api/file/trash/restore` | `id`；恢复到删除时的原位置（自动重建缺失的父目录），原位置同名即 409 |
| POST | `/api/file/trash/delete` | `id`；从回收站永久删除单条 |
| POST | `/api/file/trash/empty` | 清空回收站，返回 `cleared` 数量 |
| GET | `/api/update/check` | 查询 `update_repo` 的最新 Release；无发布版本时 `latest` 为空 |
| POST | `/api/update/apply` | `tag`；下载对应架构二进制并校验 SHA256SUMS，原子替换自身后重启服务；非 amd64/arm64 返回 501 |
| GET | `/api/logs` | `name` 可选；`lines=1..1000` 默认 200 |
| GET | `/api/firewall` | `engine=ufw或firewalld` 可选；返回状态与规则文本 |
| POST | `/api/firewall/rule` | `engine,port=1..65535,protocol=tcp或udp,action` |
| GET | `/api/sites` | 网站管理总览：`environment`（nginx/apache/docker 的安装、运行与版本）+ `items`（解析 nginx `sites-enabled`/`conf.d` 与 Apache `sites-enabled`/`conf.d` 得到的 server 块/VirtualHost，以及发布了端口的 Docker 容器） |
| POST | `/api/sites/create` | `name,engine=auto或nginx或apache或docker,mode=static或proxy,domain,port,root(静态),proxy_target(反代),image,container_port(Docker)`；原生模式由面板或 helper 生成站点配置（静态：try_files；反代：proxy_pass/ProxyPass，Apache 反代需要 mod_proxy），Debian 系写入 sites-available 并软链，RHEL 系写入 conf.d，随后重载引擎；Docker 模式以 `lightpanel-<name>` 启动带 `lightpanel.site` 标签、`--restart unless-stopped` 的容器并映射端口 |
| POST | `/api/sites/action` | `id,op=start或stop或delete或reload,engine`；Docker 仅允许对带面板标签/名称前缀的容器操作；原生删除仅允许删除含 `# managed by lightpanel` 标记的配置，删除后重载引擎 |
| GET | `/api/sites/certs` | 已签发证书列表（域名、颁发者、有效期、剩余天数、staging 标记与签发时的站点参数）及证书存储目录 |
| POST | `/api/sites/ssl` | `op=issue,id,email?,force_https?,staging?`：面板内置 ACME 客户端通过 HTTP-01 为托管站点的具体域名签发 Let's Encrypt 证书，经 helper 写入挑战文件并改写站点配置启用 HTTPS（`nginx -t`/`apachectl configtest` 失败自动回滚），签发流程为后台任务；`op=off,id` 关闭 SSL 并重载。仅支持面板创建（带托管标记）的 Nginx/Apache 站点；通配符域名需 DNS-01，暂不支持 |
| GET | `/api/apps` | 应用商店总览：`package_manager`（apt-get/dnf/yum/zypper/apk 自动探测）+ `items`（固定目录 nginx/apache/docker/mysql/mariadb/postgresql/redis 的安装、运行、版本与分组 `group`） |
| POST | `/api/apps/install` | `name`（必须是应用目录白名单键）；在后台启动安装任务并立即返回。同分组应用互斥（nginx 与 apache 同属 web 分组，一台主机只能安装一个网页服务器，被占用时返回 409）；同一应用同时只允许一个安装或卸载任务（占用中返回 409） |
| POST | `/api/apps/remove` | `name`（必须是应用目录白名单键）；在后台启动卸载任务并立即返回，APT 下为 purge（同时清除配置文件与安装失败残留），同一应用同时只允许一个安装或卸载任务（占用中返回 409） |
| GET | `/api/databases` | 数据库管理总览：`engines`（mysql/mariadb/postgresql/redis 的安装、运行与版本）+ `databases`（按引擎的库列表，每行含 `name/charset/size`，面板创建的库附带 `user/password/host` 凭据备忘）+ `users`（按引擎的用户列表，系统库/账号已过滤）+ `units`（各引擎的 systemd 单元，供启停按钮）+ `errors`（单个引擎列表失败原因） |
| POST | `/api/databases/create` | `engine,name[,user,password,charset,host]`；宝塔式一步创建：建库的同时创建同名账号并授权（`charset` 限白名单 utf8mb4/utf8/latin1/gbk/big5，PostgreSQL 固定引擎默认；`host` 为账号访问来源，缺省 localhost）。凭据备忘存入面板状态目录（0600），可在列表中查看与复制 |
| POST | `/api/databases/delete` | `engine,name`；删除数据库（不可恢复），同时清除面板中保存的凭据备忘 |
| POST | `/api/databases/user` | `engine,name,password`；创建本地用户（MySQL/MariaDB 为 `'user'@'localhost'`，PostgreSQL 为可登录角色） |
| POST | `/api/databases/user-password` | `engine,name,password`；修改用户密码；密码经 stdin 传递给客户端程序，不出现在进程参数中；同时刷新面板中相关库的密码备忘 |
| POST | `/api/databases/backup` | `engine,name`；启动后台备份任务（mysqldump/pg_dump 流式 gzip 写入 `/var/backups/lightpanel/databases/<引擎>_<库名>_<时间戳>.sql.gz`，0600），返回 `task_id` |
| GET | `/api/databases/backups` | `engine,name`；列出该库的备份（时间、大小），最新在前 |
| POST | `/api/databases/backup/restore` | `engine,name,file`；启动后台恢复任务，把备份重新导入数据库 |
| POST | `/api/databases/backup/delete` | `engine,name,file`；删除一个备份文件（文件名经严格解析校验，无法越出备份目录） |
| GET | `/api/databases/backup/download` | `engine,name,file`；流式下载备份文件 |
| POST | `/api/databases/query` | `engine,name,sql`；SQL 控制台：以管理员身份在指定库上执行一批 SQL（≤32 KiB，60 秒超时），返回 TSV 结果 |
| GET | `/api/health` | 运行诊断：UID 与模式（root/helper/普通/只读）、systemd 与系统工具可用性、helper 配置与可达性、中文告警；只读投影，不含路径与错误详情 |

普通成功返回 JSON；操作失败返回纯文本与非 2xx。400 参数非法、401 未登录、403 权限/CSRF/Host 拒绝、404 文件不存在、409 文件冲突或进程变化、413 上传过大、429 登录限流、501 工具/内核能力不支持、502 系统命令失败、503 并发满、504 命令超时。服务命令错误不会伪装成成功。

防火墙语义：

- UFW：`allow`、`deny`、`remove-allow`、`remove-deny`，变更持久保存；不会自动启用防火墙。
- firewalld：仅 `allow`、`remove-allow`，修改**默认区域的运行时规则**，重载/重启后丢弃。移除一个放行规则不是显式拒绝，故不接受 `deny`。
- 自动检测优先 firewalld，再 UFW；若同时安装，建议显式选择实际运行的引擎。

网站管理语义：

- 环境`自动识别`：探测 nginx、apache2ctl/httpd、docker 二进制及 systemd 运行状态；Docker 额外校验守护进程可达。
- 列表：解析 `sites-enabled`/`conf.d` 下的 server 块与 VirtualHost（监听端口、server_name、root、proxy_pass、SSL），并展示发布了宿主端口的 Docker 容器；未发布的容器不出现在面板里。
- 创建：原生模式支持静态站点（不存在才写入，原子替换、永不覆盖，Debian 系自动建立 sites-enabled 软链）与反向代理（nginx `proxy_pass` + 转发头，Apache `ProxyPass`/`ProxyPassReverse`，需 mod_proxy）；使用默认 `/var/www/<name>` 时自动创建目录和占位首页。Docker 模式固定参数模板启动容器，镜像引用做严格白名单校验。
- 破坏性边界：原生配置删除要求文件包含 `# managed by lightpanel` 标记且位于托管目录内；Docker 操作仅允许带 `lightpanel.site` 标签或 `lightpanel-` 名称前缀的容器。
- 一键 SSL：面板内置 ACME 客户端（`pkg/certs`，基于 `golang.org/x/crypto/acme`）直接与 Let's Encrypt 通信，全程无需 certbot 或其他外部工具。流程：helper 改写站点配置开放 ACME 挑战路径（`location ^~ /.well-known/acme-challenge/` / Apache `Alias`，指向 `/var/lib/lightpanel/acme-challenges/`）→ 挑战文件经 helper `challenge-set` 写入（root 写 0644，nginx/apache worker 可读）→ ACME 订单与 HTTP-01 验证 → 证书与元数据存入面板状态目录（`/var/lib/lightpanel/acme/certs/<域名>/`，安装脚本已建为面板用户所有；不可写时回退到用户状态目录）→ helper `ssl-apply` 写入 HTTPS 配置（443 服务器块 + 证书路径 + 可选强制 HTTPS 跳转，跳转 location 不覆盖挑战路径）并重载。`nginx -t`/`apachectl configtest` 失败时自动恢复上一版配置。每 6 小时扫描证书，到期前 30 天自动续期（Staging 证书除外）；证书列表面板展示有效期与剩余天数。
- 最小特权模式：原生建站/删除/重载/一键 SSL（配置改写、挑战文件）经 helper 的 `site` 操作（`allow_sites = true`），配置内容由 helper 用共享校验器与模板在本地重新生成，面板不传递文件内容；root 模式由面板直接执行。helper 未授权或权限不足时返回 502/403 并提示。

应用商店语义：

- 固定目录：nginx、apache、docker、mysql、mariadb、postgresql、redis；应用名必须是目录白名单键，不接受任意软件包名。部分应用在个别包管理器下无对应软件包（如 mysql 在 zypper/apk），界面会提示“当前软件包管理器不提供此应用”。
- 同类型互斥：目录中的应用带分组（nginx 与 apache 同属 `web` 分组）。同分组应用只能安装一个——安装前实时探测，已装 nginx 时安装 apache 返回 409，界面显示“已被 Nginx 占用”且不提供安装按钮；卸载后即可安装另一个。
- 包管理器自动探测：按 apt-get → dnf → yum → zypper → apk 顺序探测，安装参数由共享目录（`pkg/helper/apps.go`）按发行版重建（如 apache 在 Debian 系为 `apache2`、RHEL 系为 `httpd`），面板不传递原始 argv。
- 安装遵循官方恢复手段：apt-get 安装前先执行 `dpkg --configure -a`（修复此前失败安装留下的半配置状态，如装坏的 MySQL）并刷新软件包列表，二者失败不阻断安装本身。
- 卸载遵循官方清理手段：apt-get 先 `dpkg --configure -a` 再 `purge -y`（连同配置文件与安装失败残留一起移除），最后 `autoremove -y` 清理孤儿依赖；dnf/yum 为 `remove -y` + `autoremove -y`；zypper 为 `remove`；apk 为 `del`。收尾清理步骤失败不阻断卸载结果。
- 后台任务：安装与卸载可能持续数分钟，`POST /api/apps/install` / `POST /api/apps/remove` 启动后台任务后立即返回，前端在任务中心查看进度与包管理器输出，完成后提示“已成功安装”并引导前往对应页面（数据库类 →「数据库管理」，网页服务器/Docker →「网站管理」）；同一应用同时仅允许一个安装或卸载任务。
- 最小特权模式：安装与卸载经 helper 的 `app` 操作（`allow_apps = true`），helper 端重新探测包管理器并重建全部参数；root 模式由面板直接执行。

数据库管理语义：

- 引擎检测：探测 mysqld/mariadbd/postgres/redis-server 二进制版本与 systemd 运行状态（`postgresql@*.service` 实例单元无法用 `is-active` 探测，以 `postgresql.service` 为准）；Redis 为键值型数据库，仅提供状态查看与启停，不提供库/用户管理。
- 列表：MySQL/MariaDB 通过本机 socket 以 root 身份查询 `information_schema`（库名、默认字符集与数据+索引容量）与 `mysql.user`（依赖 root 的 socket/uvp 认证，Debian/Ubuntu 与 RHEL 系 MariaDB 默认满足）；PostgreSQL 通过 `runuser -u postgres -- psql` 查询库名、编码与 `pg_database_size`。`information_schema`/`mysql`/`performance_schema`/`sys` 与系统账号不出现在列表中。
- 一步建库（宝塔式）：添加数据库时同步创建账号并 `GRANT ALL PRIVILEGES`（MySQL 家族为一条 stdin SQL 批处理；PostgreSQL 为 `CREATE ROLE` + `CREATE DATABASE ... OWNER`，开启 `ON_ERROR_STOP`，失败不留半成品）。字符集限白名单（PostgreSQL 固定引擎默认——修改编码需要 template0 与匹配的 locale，面板不提供），访问来源可选 localhost / 所有 IP / 自定义网段。面板把创建的账号、密码与访问来源存入状态目录（`/var/lib/lightpanel/db/credentials.json`，0600，回退用户状态目录），列表中可随时查看、复制；改密后备忘同步刷新，面板外创建的库不显示备忘。
- 备份/恢复：`mysqldump --single-transaction --quick --routines --events`（MariaDB 优先 `mariadb-dump`）或 `pg_dump` 的输出在进程内流式 gzip 写入固定目录 `/var/backups/lightpanel/databases`（0700/0711，文件 0600；helper 模式下把文件属主改为面板用户，使非特权面板可流式下载而目录不可列举）。备份与恢复都是后台任务；文件名 `<引擎>_<库名>_<时间戳>.sql.gz` 经双向解析校验（引擎、库名、时间戳逐段重验证），不存在路径穿越；恢复时在进程内解压流式喂给客户端，两条客户端链路都开启“遇错即停”。
- SQL 控制台：以管理员身份在指定库执行一批 SQL（mysql `--batch` / psql `-A -F`，输出为带表头的 TSV，前端解析展示前 200 行）。面板本身即管理工具（另有 root 网页终端），SQL 不做语句级过滤；单批 ≤32 KiB、60 秒超时、输出上限 1 MiB。
- 建库/删库/用户：数据库名限 `[a-zA-Z0-9_]`（1–63 字符），用户名限 `[a-zA-Z0-9_.-]`（1–32 字符）；全部 argv 由共享模块（`pkg/helper/databases.go`）在面板与 helper 两端重建。密码只经 stdin 渲染进 SQL，绝不出现在进程参数；含引号、反斜杠与 Unicode 的密码会被正确转义，但不允许控制字符。
- 启停：数据库页的启动/停止/重启走系统服务页同一套 `POST /api/service/action`；`helper.services` 缺省通配放行所有单元，收紧模式下需加入对应单元（如 `mysql.service`、`postgresql.service`、`redis-server.service`）。
- 最小特权模式：列表、变更、备份与控制台经 helper 的 `database` 操作（`allow_databases = true`），helper 端重新校验引擎、名称与密码并重建全部命令；备份/恢复连接与子进程各有 30/29 分钟上限，任务中心可查进度；root 模式由面板直接执行。

CPU/网络首次请求用于建立基线，后续返回采样间隔平均值；共享缓存最多每 2 秒采样一次。网络是非 loopback 接口汇总，虚拟网卡可能重复计数；磁盘显示根分区。进程只展示 UID、名称、状态、RSS，不收集可能包含密码的完整命令行。

## 3. 完整可运行代码

源代码已按上面的模块直接落盘，所有处理器、模板和样式均已实现；没有依赖未提供的 partial 模板或静态文件。直接在本目录构建即可。

关键安全约束：

- 无默认密码；缺失/无效密码哈希、未知 TOML 字段和无效环境变量使启动失败。
- bcrypt cost 接受 10–14；CLI 默认 12，密码输入 12–72 字节且二次确认，不通过参数、日志传递明文。
- session 最多 128 个，失败 IP 表最多 1024 项，过期项在登录时清理；bcrypt 同时只执行一次；单 IP 15 分钟内 5 次失败后拒绝。
- session cookie 为 HttpOnly + SameSite=Strict；HTTPS origin 下增加 Secure。
- 全局最多 8 个在途处理器；普通请求体 16 KiB，登录 4 KiB，上传 32 MiB。上传/下载均流式。
- 系统工具只从 `/usr/sbin /usr/bin /sbin /bin` 查找白名单程序；参数校验、固定环境、8 秒超时、1 MiB 输出上限。服务操作不自动重试，超时后应检查实际服务状态。
- 单管理员可设 `read_only=true`；写 API 在服务端拒绝，不能仅靠前端按钮绕过。
- 文件/日志读取与所有已认证变更记录 JSON 审计，拒绝请求也记录；不记录密码、session、CSRF 和文件内容。stdout 默认交给 journald 管理保留策略；直接文件日志需外部轮转并重启进程重开日志。
- SIGINT/SIGTERM 等待在途请求最多 10 秒，随后强制关闭；WebSocket 终端会话随即以 `server_shutdown` 关闭。

## 4. 编译与部署

### 一键部署（远程 Linux 服务器）

在目标服务器（Ubuntu/Debian/CentOS/Rocky/Alma，amd64/arm64，systemd，仅需 root、curl 或 wget 与 tar）上执行：

```bash
curl -fsSL https://raw.githubusercontent.com/acleverfreebird/lightpanel-go/main/scripts/install.sh -o install-lightpanel.sh
sudo bash install-lightpanel.sh
```

脚本自动完成：架构检测 → 下载源码现场编译（缺失 Go 工具链时自动安装到 `/usr/local/go`，模块代理失败自动切换 goproxy.cn）→ 安装到 `/opt/lightpanel` → 写入 `config.toml` → 创建系统用户 `lightpanel` → 注册并启动 systemd 服务（面板以非特权用户运行，`lightpanel-helper` 以 root 执行白名单特权操作）。

要点：

- 因为需要交互输入管理员密码（12–72 字节，不回显、不落盘明文），请使用上面两步式命令，而不是 `curl … | sudo bash` 管道形式。
- 重复执行同一命令即为升级：替换二进制并重启服务（面板与 helper 一并重启），保留现有 `config.toml` 与密码。新版本新增的配置项会在服务启动时自动补全进 `config.toml`：已有键的值与注释原样保留，缺失条目连同注释追加（原配置备份为 `config.toml.bak`）；缺省策略为「部署即全功能」，未显式写入的 `allow_*` 开关与 `services` 白名单在运行时按开启处理，升级时缺失条目也会显式补全为 `true`。面板内的「在线更新」同样如此。
- 非交互环境（自动化脚本）预置哈希：`curl -fsSL …/install.sh | sudo LP_PASS_HASH='<bcrypt 哈希>' bash -`；哈希先用 `lightpanel -hash-password` 在有终端的机器上生成。
- 反向代理/域名访问：`sudo bash install-lightpanel.sh --origin https://panel.example.com`（面板默认监听 `0.0.0.0`，反代场景也可自行将配置中的 `host` 改回 `127.0.0.1`，TLS 由反代终止）。
- 其他选项：`--port 8888`、`--admin NAME`、`--read-only`、`--release latest`（改用 GitHub Release 预编译二进制，含 sha256 校验）、`--ref TAG`、`--force-config`（重写配置）、`--no-start`、`--legacy-root`（旧版 root 面板模式，不创建专用用户/不启用 helper）；完整列表见 `sudo bash install-lightpanel.sh --help`。
- 版本号注入：源码编译安装会自动注入版本号（`--ref` 为标签时用标签名；默认 `main` 分支时查询 GitHub 最新 Release 的标签，查询失败退回 `main`），也可用 `sudo LP_VERSION='v1.2.3' bash install-lightpanel.sh` 显式指定。版本号影响面板内「检查更新」的版本对比，`dev` 版本会跳过对比。
- GitHub 直连不畅时：`sudo LP_SOURCE_MIRROR='https://ghproxy.example/' bash install-lightpanel.sh`，源码/Release 下载会先尝试镜像前缀再回退官方地址（Go 工具链已内置 golang.google.cn 与阿里云镜像回退，Go 模块代理失败自动切换 goproxy.cn）。
- 卸载：`curl -fsSL https://raw.githubusercontent.com/acleverfreebird/lightpanel-go/main/scripts/uninstall.sh | sudo bash`（加 `--purge` 一并删除 `/var/lib/lightpanel` 数据目录）。

### 发布与在线更新

- 发布流程：推送 `v*` 标签（如 `git tag v0.1.0 && git push origin v0.1.0`），GitHub Actions 自动构建 amd64/arm64 静态二进制（版本号注入 `-X main.version`）、生成 `SHA256SUMS` 并发布到该标签的 Release。
- 面板内更新：概览页「版本与更新」→「检查更新」对比当前版本与最新 Release；确认后「更新到最新版」下载对应架构二进制，先校验 SHA256 再原子替换自身二进制，随后自动 `systemctl restart lightpanel`。最小特权模式下由 helper 复核并安装（见上文）；root 模式由面板直接替换。整个流程需要登录、带 CSRF 校验、计入审计，只读模式不可用。
- GitHub 直连不畅时配置 `update_mirror`（或 `LP_UPDATE_MIRROR`）为镜像站点源（如 `https://ghproxy.family`），下载会走 `镜像 + 官方地址` 前缀；API 查询仍直连 GitHub。
- 安全权衡：校验和来自同一 Release，防下载损坏但不防 Release 本身被篡改；上游仓库安全即更新安全。不想使用时可忽略该卡片，继续用 install.sh 升级。

### 编译（运行服务器无需 Go）

使用最新维护版 Go（最低 1.25），无需 C 工具链：

```bash
go mod download
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/lightpanel-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/lightpanel-linux-arm64 .
```

Windows PowerShell 交叉编译：

```powershell
$env:CGO_ENABLED='0'
$env:GOOS='linux'
$env:GOARCH='amd64'
go build -trimpath -ldflags='-s -w' -o dist/lightpanel-linux-amd64 .
$env:GOARCH='arm64'
go build -trimpath -ldflags='-s -w' -o dist/lightpanel-linux-arm64 .
Remove-Item Env:CGO_ENABLED,Env:GOOS,Env:GOARCH
```

Linux 本地验证（竞态检测的测试工具链需要 gcc，不影响发布产物）：

```bash
go test -race ./...
go vet ./...
```

### 首次配置与启动

```bash
sudo install -d -m 0750 /opt/lightpanel /var/lib/lightpanel/files
sudo install -m 0755 dist/lightpanel-linux-amd64 /opt/lightpanel/lightpanel
sudo install -m 0600 config/example.toml /opt/lightpanel/config.toml
/opt/lightpanel/lightpanel -hash-password
# 将输出的 bcrypt 哈希填入 /opt/lightpanel/config.toml 的 password_hash。
# 密码不会显示，也不会提供任何预置账号密码组合。
sudo /opt/lightpanel/lightpanel -c /opt/lightpanel/config.toml
```

默认监听 `0.0.0.0:8888` 并放行明文 HTTP 对外（`allow_public_http` 缺省开启）：服务器上任意网卡 IP 都可以直接访问 `http://<服务器IP>:8888`（公网访问请确认云安全组/防火墙已放行端口）。绑定 `0.0.0.0` 且未配置域名 `public_origin` 时为通配模式，Host/Origin 校验按请求自身的 Host 放行，经由任意 IP 访问均可。

如需仅本机访问（更安全），将配置改为 `host = "127.0.0.1"` 与 `allow_public_http = false` 并重启服务；此时远端可用 SSH 隧道：

```bash
ssh -N -L 8888:127.0.0.1:8888 user@your-server
```

仍在本地打开 `http://127.0.0.1:8888`，保持与 `public_origin` 相同。不要用 `localhost` 替代 `127.0.0.1`，除非同时修改配置。

TOML 是可选外部文件：不传 `-c` 时只用安全默认值和环境变量。全部支持覆盖：`LP_HOST`、`LP_PORT`、`LP_ADMIN_USER`、`LP_PASS_HASH`、`LP_TLS_CERT`、`LP_TLS_KEY`、`LP_PUBLIC_ORIGIN`、`LP_LOG_FILE`、`LP_READ_ONLY`、`LP_ALLOW_PUBLIC_HTTP`、`LP_MAX_UPLOAD_MB`（1..2048，默认 32）、`LP_UPDATE_REPO`（默认 `acleverfreebird/lightpanel-go`）、`LP_UPDATE_MIRROR`（http(s) 源，留空直连 GitHub）、`LP_HELPER_SOCKET`（已配置 `[helper]` 段时覆盖 socket 路径）。`sandbox_root` 配置项已废弃（文件管理现为全盘访问），旧配置文件中的该字段会被忽略。生产配置/哈希/私钥应仅允许运行账号读取。

### HTTPS 与反向代理

直接 TLS：设置 `tls_cert`、`tls_key` 为 PEM 路径、`public_origin="https://panel.example.com:8888"`（默认已监听 `0.0.0.0`，无需另设 `host`）。证书续期后重启面板加载；站点证书由「网站管理」的内置 ACME 一键签发与自动续期，面板自身 TLS 的证书可复用签发结果或由现有反代/证书工具管理。

反代 TLS：TLS 两项留空，`public_origin="https://panel.example.com"`。面板默认绑定 `0.0.0.0` 并放行明文 HTTP 对外（`allow_public_http` 缺省开启，可用环境变量 `LP_ALLOW_PUBLIC_HTTP` 覆盖）；确需对外明文监听的场景应自行评估——公网明文传输会暴露凭据与会话，生产环境仍应使用 TLS 或反向代理。如需收紧：将 `host` 改回 `127.0.0.1` 并设 `allow_public_http = false`，此时无本地证书时仅允许 loopback 监听。绑定 `0.0.0.0` 且未配置域名 `public_origin` 时，浏览器经由实际 IP 访问，Host/Origin 校验按请求自身的 Host 放行（跨站请求仍被 Origin/Referer 与 CSRF token 拦截）；若配置了具体域名/IP 的 `public_origin`，则维持严格 Host 校验，只能经该 origin 访问。Nginx HTTPS server 内示例：

```nginx
location /ws/terminal {
    proxy_pass http://127.0.0.1:8888;
    proxy_set_header Host $http_host;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 86400s;
    proxy_buffering off;
}

location / {
    proxy_pass http://127.0.0.1:8888;
    proxy_set_header Host $http_host;
    proxy_http_version 1.1;
    client_max_body_size 32m;
    proxy_read_timeout 65s;
    proxy_request_buffering off;
    proxy_buffering off;
}
```

外部 server 需另配 `listen 443 ssl`、有效证书及 HTTP→HTTPS 跳转。面板不从客户端提供的 forwarded headers 推导可信来源，也不依赖代理头设置 Secure cookie。多个面板实例各自维护 session，不支持无粘性会话负载均衡。

### systemd

默认单元以专用非特权用户 `lightpanel` 运行（一键部署自动创建），root 级操作由独立的 `lightpanel-helper` 服务按白名单执行，见下节「最小特权 helper」。手动部署时安装两个单元：

```bash
sudo install -m 0644 lightpanel.service lightpanel-helper.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now lightpanel-helper lightpanel
sudo systemctl status lightpanel lightpanel-helper
sudo journalctl -u lightpanel -n 100 --no-pager
```

面板进程不需要任何 Linux capability；日志读取（journalctl）需要 `systemd-journal` 组（单元中已声明 `SupplementaryGroups=systemd-journal`，无该组的系统可删除此行，代价是日志页返回错误）。需要旧版 root 面板行为时把单元 `User` 改回 root（或用 `--legacy-root` 安装），此时 `[helper]` 不参与。

单元启用 `NoNewPrivileges`、内核参数保护等约束。文件管理经 helper 以 root 执行后，面板自身只需读写其状态/日志/更新暂存目录，但下载/上传/编辑的文件内容仍流经面板进程转发，如需进一步收紧可结合实际部署路径启用 `ProtectSystem`/`ReadWritePaths` 等隔离；`/proc`、`/sys`、`/dev`、`/run` 的删除与重命名由面板与 helper 双侧拒绝。helper 单元也不启用文件系统写隔离与 `RestrictSUIDSGID`：它会派生系统包管理器（应用商店）并写入站点与证书状态，子进程完整继承单元沙箱，`ProtectSystem=strict` 会让 `/var/lib/apt` 只读、`RestrictSUIDSGID` 会拦截 apt 降权到 `_apt` 用户的 `setresuid`，二者均使安装必然失败；helper 的特权收敛依赖白名单 argv（应用名/包名/站点配置全部由 helper 端目录重建），`NoNewPrivileges`、`PrivateTmp` 与内核防护项保留。

### 最小特权 helper（默认）

最小特权模式把「面向网络的 HTTP 进程」与「执行特权操作的最小面」分离：

- **面板进程**（`lightpanel.service`）以系统用户 `lightpanel` 运行，没有任何 capability。HTTP/TLS/表单解析等全部攻击面都运行在该用户权限下。
- **helper 进程**（`lightpanel-helper.service`，即同一二进制的 `lightpanel helper -c …` 子命令）以 root 运行，监听 `/run/lightpanel/helper.sock`。它只接受一个固定的小请求集，执行前做三重校验：连接方 UID（每条连接用 `SO_PEERCRED` 复核，仅 `allowed_users` 与 root）、操作白名单、参数重建（例如防火墙参数由 helper 按端口/协议白名单重新生成，不接受面板转发的原始 argv）。

`config.toml` 的 `[helper]` 段同时是两端的授权边界：

```toml
[helper]
allowed_users = ["lightpanel"]
allow_firewall = true   # ufw/firewalld 端口规则（读+写）
allow_kill = true       # 对任意进程发 SIGTERM/SIGKILL（进程身份经 pidfd 固定）
allow_update = true     # 在线更新：helper 复核 SHA256 后安装并重启面板
allow_sites = true      # 托管站点：配置写入（helper 端重新生成内容）、托管删除、引擎重载、一键 SSL
allow_apps = true       # 应用商店：经系统软件包管理器安装/卸载固定目录中的应用（参数在 helper 端重建）
allow_databases = true  # 数据库管理：列表/建库/删库/用户管理（密码仅经 stdin，参数在 helper 端重建）
allow_terminal = true   # 网页终端：中继 root PTY，终端页直接进入 root 登录 shell
allow_files = true      # 文件管理：全盘浏览/下载/上传/编辑/删除等以 root 执行（路径与参数 helper 端重新校验）

# 按服务/动作细分授权：单元名 = 允许的 systemd 动作（start/stop/restart）。
# 缺省通配放开所有单元；如需收紧，把 "*" 行换成逐个单元，空表 = 全部拒绝。
[helper.services]
"*" = ["start", "stop", "restart"]
```

以上开关缺省全部开启（配置文件未写入即开启，与「部署即全功能」的缺省策略一致）。升级后新增的开关会显式补全为 `true` 进 `config.toml`；如需收紧，把对应值改为 `false`（或把 `[helper.services]` 换成逐单元授权/空表）并重启 `lightpanel-helper` 即可生效。

行为细节：

- 面板以 root 运行时（旧模式）完全忽略 `[helper]`，行为与旧版本一致；非 root 且未配置 `[helper]` 时，服务控制/防火墙/进程信号/在线更新返回明确错误，其余只读功能正常。
- 网页终端（`allow_terminal`）：终端页始终进入 root 登录 shell——非 root 面板把 PTY 中继给 helper 派生，会话随面板连接断开或登录撤销即时回收；helper 未授权（`allow_terminal = false`）、未运行或协议过旧时终端以明确错误结束，不静默降级。详见 [终端](docs/WEB-TERMINAL.md)。
- 服务列表、服务详情、日志读取等只读操作不经 helper。
- 文件管理（`allow_files`）：全部接口经 helper 以 root 执行——浏览、下载、上传、在线编辑、新建、重命名、删除与改权限作用于整个文件系统（宝塔式 root 文件管理），root 属主的文件不再报 403。路径与参数在 helper 端重新校验，下载/上传/保存的内容经中继流式传输（上传与保存以提交帧收尾，传输中断不会落下半截文件）。设为 `false` 时回退为面板用户自身权限，root 属主文件的写操作将返回 403。
- 在线更新：非 root 面板把 Release 资产下载到 `staging_dir`（默认 `/var/lib/lightpanel/update`），helper 复核目录属主/权限与 SHA256 清单后再换二进制并延迟重启面板；暂存目录必须属于面板用户且不允许组/其他用户可写。
- 托管站点（`allow_sites`）：面板不发送文件内容——创建请求只带参数（名称、引擎、类型、域名、端口、根目录/反代目标），helper 用与面板完全一致的共享校验器和模板在本地重新生成配置；删除要求目标位于托管目录且含 `# managed by lightpanel` 标记；引擎重载与一键 SSL 的配置改写/挑战文件写入（`allow_sites` 整体开关，不做单元级细分）也在 helper 内完成，ACME 网络交互由面板自身完成、不依赖外部工具。站点校验原语（`pkg/helper/sites.go`）两端共享，保证判定一致。
- helper 每次操作写 journald 审计日志（操作、UID、对象、结果）。

`--legacy-root` 保留旧模式；升级已有安装时脚本会在配置缺失 `[helper]` 段时追加通配授权（等价旧版行为），建议随后按需收紧。

支持采用 systemd 的 Ubuntu/Debian/CentOS/Rocky/Alma 的 Linux amd64/arm64；尚未在每个发行版上逐一实机认证。安全结束进程依赖 Linux **5.3+ pidfd**；旧 CentOS 7 内核的概览/文件等可用，但进程结束返回 501，不退回有 PID 复用竞态的 `kill(pid)`。工具缺失时对应模块显示明确错误，其余模块可用。发行版 SELinux/Polkit/系统命令版本可能要求额外策略适配。

## 5. 资源占用预估与优化建议

目标为小型服务器低并发下常驻 20–50 MiB 以内，**不能将 Go 软内存限制误认为 RSS 上限**。本次 WSL 轻载浏览器验证曾观测约 10–12 MiB，详细可复现结果见 `docs/VALIDATION.md`。二进制约 8–9 MiB，真实体积随 Go 版本/架构变化。

- 默认 `GOMEMLIMIT=40MiB` 等效软限制、GOGC=75；显式环境变量优先。调低 GOGC 可降低堆峰值，但会增加 GC CPU。
- 指标无常驻采样 goroutine；概览可见时每 3 秒轮询，隐藏页面停止轮询。系统服务/日志/文件按需读取。
- 文件流式传输，目录每页 200 项，进程每页 100 项；进程搜索仍需扫描 `/proc`，进程数量很多时开销会增加。
- systemctl/journalctl/firewall 命令的短暂子进程会额外消耗内存，systemd 的 cgroup 内存计量包括这些子进程。示例 MemoryHigh=64M、MemoryMax=128M 是容错保护，不是常驻目标。
- 不默认用 UPX：省磁盘不等于省 RSS，且会改变启动行为和安全扫描结果。
- 用 `/proc/<pid>/status`、`ps -o rss` 和 cgroup 指标实测；不要用虚拟地址空间 VSZ 判断常驻内存。并发、大目录、长日志、TLS 和恶意请求应单独压测。

## 6. 后续可扩展点

1. （已实现，见「最小特权 helper」）后续收窄方向：按动作区分面板管理员角色、helper 请求限速与审计导出。
2. 多用户/角色、TOTP、会话撤销页面；确有需要时才引入 SQLite。
3. 防火墙区域选择、持久化规则事务、远程连接保护和定时回滚。
4. 网页终端已是完整交互式终端（登录即用）；如需再收紧，可叠加会话配额、时长上限或专用跳板机审计。
5. 软件包、cron、网络配置、Docker 检测、告警；各自独立适配器，保持依赖可选。
6. 多磁盘/网卡分项、进程 CPU 采样、超大进程表分页优化、发行版兼容矩阵与 CI。
