# Web Terminal 安全边界与部署

Web Terminal 是独立的高风险模块，缺省 `terminal_enabled = true`。显式配置 `false` 或 `LP_TERMINAL_ENABLED=false` 可关闭；只读模式始终拒绝连接。入口位于侧栏「Web Terminal」，须主动确认后连接，离开页面或关闭标签会断开，不自动重连。

## Origin 与认证

将 `public_origin` 配置为浏览器实际访问的固定地址，例如 `https://panel.example.com`。即使模块开启，`http://0.0.0.0:8888` / IPv6 通配地址也不能创建终端。终端严格比较请求 Host 和唯一的 Origin，不接受缺失 Origin、`null`、Referer 替代、尾斜杠、不同 scheme/port 或转发头推断。其他面板模块仍遵循原有配置策略。

1. 已登录的写权限管理员向 `POST /api/terminal/ticket` 发送 `X-CSRF-Token`，返回 `{ "ticket": "...", "expires_in": 30 }`，响应禁止缓存。
2. 浏览器向 `GET /ws/terminal` 升级，携带原登录 cookie、严格 Origin 和子协议 `lightpanel-terminal, lp-ticket.<ticket>`。服务器只选择 `lightpanel-terminal`，不反射票据；不要把票据放入 URL。
3. 256-bit 随机票据只在内存保存 SHA-256 摘要，绑定签发的登录会话；30 秒有效，兑换时原子删除。重复、过期、跨登录会话的票据均拒绝；升级失败或额度已满也不会退还票据。
4. 每个登录会话只保留最新票据，最多保存 128 张，签发时清除过期条目。退出登录、替换登录和重启会撤销相关访问；正在运行的终端会收到登录撤销通知。

反向代理须保留 Host，转发 WebSocket Upgrade，并将读取超时设为至少 30 分钟。使用 HTTPS/WSS；不要在代理日志里记录 Cookie、CSRF 或 `Sec-WebSocket-Protocol` 请求头。

## 固定硬上限

| 边界 | 上限 |
|---|---|
| PTY 数量 | 全局 4；单管理员 2（所有登录会话共享） |
| 握手时间 | 5 秒 |
| 单次 WebSocket 消息 | 4096 字节 |
| 单会话累计输入 / 输出 | 1 MiB / 16 MiB；超过即关闭 |
| 窗口尺寸 | 1–100 行、1–240 列；初始 24×80 |
| 无输入超时 | 5 分钟；输出、ping、resize 不延长 |
| 最长会话 | 30 分钟，且不超过登录有效期 |
| 向客户端写入等待 | 5 秒 |
| 正常 shell 退出后的输出排空 | 最长 100 毫秒，仍响应撤销/超时 |
| 浏览器显示缓冲 | 最近 65536 字符，安全文本显示 |

原生 PTY 仅运行固定 `/bin/sh -i`，工作目录 `/`，受控环境变量，不继承面板的 `LP_PASS_HASH` 等环境值。二进制 WebSocket 消息为原始输入/输出字节；文本消息仅接受 `{"type":"resize","rows":24,"cols":80}`。浏览器提供纯文本输入、Enter、Ctrl+C、Ctrl+D，采用 `TERM=dumb`，不支持 vim/top 等全屏终端应用。

关闭连接、输入/输出超限、超时、登出、shell 退出和服务关闭均清理 PTY、socket 与同一 PTY session 的进程（包括其他作业控制进程组），等待 shell 回收后归还额度。需要 Linux pidfd 支持及可访问的 `/proc`；pidfd 不可用时拒绝启动，不退化为只清理 shell 的实现。服务退出显式关闭 hijacked WebSocket，因为 `http.Server.Shutdown` 不管理它们。

## 权限、资源与审计的实际范围

终端以**面板运行用户**身份执行，默认 systemd 部署为 `lightpanel`。没有新增 helper shell 接口，也不通过 helper 升权。面板以 root 部署时，终端就是 root 权限。

这不是恶意代码沙箱：拥有 shell 权限的管理员可以启动主动 `setsid` 脱离 PTY session 的进程，也可访问该系统用户有权限读取的文件。PTY 会话上限、字节上限和超时不等同于任意子进程的独立 CPU/内存隔离。推荐使用仓库的 `lightpanel.service`：它提供整个面板服务共享的 `TasksMax=64`、`MemoryMax=128M`、`LimitNOFILE=1024` 和 `NoNewPrivileges=true`；直接运行二进制时这些 systemd 上限不生效。若要求对不可信命令实施独立资源隔离，应使用容器/专用 cgroup 执行器。

`terminal_audit` 记录签发、拒绝、启动失败、开始、结束，以及账号、实际网络 peer、随机连接 ID、PID/UID、持续时间、输入输出字节数、关闭原因。认证/CSRF 拒绝另由现有 `request_rejected` 记录。无命令正文、输出正文、票据、session cookie 或密码；因此这是生命周期审计，不是命令录像。默认进入结构化日志/journald，可沿用 `log_file` 收集。需要防篡改留存时由部署侧将日志转发至独立存储。

## 验证

`go test -race ./...` 覆盖 Origin/权限/CSRF、一次性及跨会话票据、并发额度、真实 PTY 输入输出、尺寸和消息上限、累计字节上限、输出不能续期、退出登录、服务关闭、进程清理、最终输出和审计脱敏。`node --test scripts/*.test.mjs` 覆盖前端禁用状态、票据传输、缓冲上限与异步连接取消。

2026-09-29 验证结果（WSL Ubuntu 24.04 / Go 1.27.1）：全仓库 race 测试、`go vet ./...`、`go mod verify`、Linux amd64/arm64 静态构建通过；13 项前端测试通过。HTTP 冒烟通过，包括文件往返、审计检查与正常关闭。真实浏览器验证连接确认、命令往返（UID 1000）、离开终端页面断连，控制台无错误；临时实例已关闭。arm64 仅交叉构建，未在 ARM 硬件执行。

独立代码审查指出的退出输出排空、pidfd 能力不足拒绝、PTY 描述符原子 CLOEXEC 三项问题均已修复。
