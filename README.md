# ZZMRClone Manager

基于 Web 的 Rclone 自动化管理工具，支持任务调度、目录监控、实时日志、结构化转移记录、**云盘本地挂载**、**OpenList 目录自动刷新** 和 **Webhook 一次性下载 API**。提供可视化界面和持久化数据库，一条命令即可部署。

---

## 功能特性

### Rclone功能

- **任务管理** — 创建、编辑、启停、删除 Rclone 传输任务
- **目录监控** — 实时监听源目录变化，文件新增后自动触发传输
- **定时执行** — 支持按固定间隔（分钟）自动执行任务
- **实时日志** — WebSocket 推送任务执行日志，支持倒序查看和关键字高亮
- **结构化转移记录** — 每条文件传输自动生成持久化记录，支持分页查询和筛选
- **云盘本地挂载** — 参考 CloudDrive2 的使用方式，将远程云盘挂载到容器本地目录
- **Webhook 下载 API** — 外部 POST `/webhook` 后立即入队，后台执行 `rclone copy` + `rclone check`，成功后依次回调 `callback_url` 和 `curl_url`

### OpenList 集成

- **自动目录刷新** — 任务转移成功后，自动调用 OpenList API 刷新目标目录缓存
- **路径映射** — 支持配置路径映射关系，解决 OpenList 挂载路径与 rclone 目标路径不一致的问题
- **刷新状态追踪** — 转移记录中展示每条文件的 OpenList 刷新结果（成功/失败）

### 安全特性

- **随机初始密码** — 首次部署自动生成随机管理员密码，告别硬编码默认密码
- **密码重置命令** — 内置 `--reset-password` CLI 命令，随时重置管理员密码
- **密码文件** — 初始/重置密码自动写入 `data/initial-password.txt`，方便查找
- **Token 保护** — API 支持 Token 鉴权，防止未授权访问

---

## 技术栈

| 层级 | 技术 |
|------|------|
| 前端 | React 18 + Tailwind CSS + Lucide Icons |
| 后端 | Go + Gin + GORM |
| 数据库 | SQLite（自动迁移，零配置） |
| 任务调度 | robfig/cron |
| 目录监控 | fsnotify |
| 实时通信 | WebSocket |

---

## 快速开始

### Docker 部署


```bash
# 1. 下载 api 分支 docker-compose.yml
wget https://raw.githubusercontent.com/great99mm/zzmrclone-manager/api/docker-compose.yml

# 2. 编辑 docker-compose.yml，配置 rclone 远端、Webhook Token、白名单、本地目录映射
vim docker-compose.yml

# 3. 启动
docker compose up -d
```

> 当前 api 分支统一对外端口为 `6050`：WebUI、`/api`、`/webhook` 都走 `http://ip:6050`。云盘挂载功能依赖 FUSE，`docker-compose.yml` 已包含 `/dev/fuse`、`SYS_ADMIN`、`apparmor:unconfined`；宿主机目录映射请按你自己的路径自行添加。


### 获取管理员密码

首次部署时系统会自动生成随机密码，通过以下方式获取：

```bash
# 方式一：在宿主机直接查看（data 目录已挂载）
cat data/initial-password.txt

# 方式二：查看容器内密码文件
docker exec rclone-manager cat /app/data/initial-password.txt

# 方式三：查看后端日志
docker exec rclone-manager cat /app/logs/backend.log | grep -A5 "INITIAL ADMIN"
```

访问 `http://ip:6050`，使用用户名 `admin` 和上述方式获取的密码登录。

### 重置管理员密码

```bash
# 方式一：使用 Makefile
make reset-password

# 方式二：直接执行命令
docker exec rclone-manager /app/server --reset-password
```

执行后新密码会打印到屏幕并自动写入 `data/initial-password.txt`。

---

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `RCLONE_MANAGER_DATA_DIR` | `/app/data` | 数据目录（SQLite 数据库） |
| `RCLONE_MANAGER_LOG_DIR` | `/app/logs` | 日志文件目录 |
| `RCLONE_MANAGER_PORT` | `6050` | HTTP 服务端口 |
| `RCLONE_CONFIG` | `/root/.config/rclone/rclone.conf` | Rclone 配置文件路径 |
| `RCLONE_MANAGER_API_TOKEN` | `""` | API Token（空表示不启用） |
| `RCLONE_MANAGER_MOUNT_ROOT` | `""` | 可选：限制挂载目录根路径；留空表示不限制 |
| `RCLONE_MANAGER_WEBHOOK_LOCAL_BASE_DIR` | `/app/data/downloads` | Webhook 下载本地根目录 |
| `RCLONE_MANAGER_WEBHOOK_RCLONE_PATH` | `rclone` | rclone 可执行文件路径 |
| `RCLONE_MANAGER_WEBHOOK_RCLONE_REMOTE` | `""` | Webhook 下载使用的 rclone 远端名，必填后任务才可执行 |
| `RCLONE_MANAGER_WEBHOOK_TOKENS` | `""` | Webhook Token，多个用逗号分隔 |
| `RCLONE_MANAGER_WEBHOOK_ALLOWED_CALLBACK_HOSTS` | `""` | callback_url 域名白名单，多个用逗号分隔，支持 `*.example.com` |
| `RCLONE_MANAGER_WEBHOOK_ALLOWED_CURL_HOSTS` | `""` | curl_url 域名白名单，多个用逗号分隔，支持 `*.example.com` |
| `RCLONE_MANAGER_WEBHOOK_ALLOW_ANONYMOUS` | `false` | 是否允许 `/webhook` 不带 Token |
| `RCLONE_MANAGER_WEBHOOK_WORKERS` | `2` | Webhook 下载 worker 数 |
| `RCLONE_MANAGER_WEBHOOK_QUEUE_SIZE` | `100` | Webhook 队列长度 |
| `RCLONE_MANAGER_WEBHOOK_TRANSFERS` | `4` | rclone transfers |
| `RCLONE_MANAGER_WEBHOOK_CHECKERS` | `8` | rclone checkers |
| `RCLONE_MANAGER_WEBHOOK_RETRIES` | `3` | rclone retries |
| `RCLONE_MANAGER_WEBHOOK_LOW_LEVEL_RETRIES` | `10` | rclone low-level retries |
| `RCLONE_MANAGER_WEBHOOK_BWLIMIT` | `""` | rclone bwlimit，可选 |
| `RCLONE_MANAGER_WEBHOOK_JOB_TIMEOUT` | `0s` | 单个 Webhook 任务超时，`0s` 表示不限制 |
| `RCLONE_MANAGER_WEBHOOK_HTTP_TIMEOUT` | `30s` | callback/curl_url 请求超时 |
| `RCLONE_MANAGER_WEBHOOK_MAX_RCLONE_LOG_BYTES` | `1048576` | 单个任务保留的 rclone 日志最大字节数 |

---

## Webhook 下载 API

### 配置要点

至少配置：

```yaml
environment:
  - RCLONE_MANAGER_PORT=6050
  - RCLONE_CONFIG=/root/.config/rclone/rclone.conf
  - RCLONE_MANAGER_WEBHOOK_RCLONE_REMOTE=webdav
  - RCLONE_MANAGER_WEBHOOK_LOCAL_BASE_DIR=/app/data/downloads
  - RCLONE_MANAGER_WEBHOOK_TOKENS=replace-with-long-random-token
  - RCLONE_MANAGER_WEBHOOK_ALLOWED_CALLBACK_HOSTS=sender.example.com
  - RCLONE_MANAGER_WEBHOOK_ALLOWED_CURL_HOSTS=localhost,127.0.0.1,api.example.com
```

`RCLONE_MANAGER_WEBHOOK_RCLONE_REMOTE` 只写远端名，不带冒号；例如 `webdav`，服务会拼成 `webdav:/remote/folder/a`。

### 调用示例

外部系统调用：

```bash
curl -X POST http://ip:6050/webhook \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <webhook-token>' \
  -d '{
    "path": "/remote/folder/a",
    "callback_url": "https://sender.example.com/download-finished",
    "curl_url": "http://localhost:5244/api/fs/list?path=/&refresh=true",
    "curl_headers": {
      "Authorization": "openlist-xxx"
    }
  }'
```

`curl_headers` 可选，用于需要额外请求头的刷新接口，例如 OpenList：

```bash
curl -X GET "http://localhost:5244/api/fs/list?path=/&refresh=true" \
  -H "Authorization: openlist-xxx"
```

返回 `202 Accepted`：

```json
{"job_id":"job_xxx","job_type":"one_time","status":"pending"}
```

这是一次性任务。任务会写入 SQLite，并在 WebUI 左侧 **Webhook 下载** 页面以任务卡片展示；点击卡片 **详情** 可查看远端路径、本地路径、callback/curl URL、curl headers、错误和 rclone 日志。

管理接口：

- `GET /api/webhook-jobs`
- `POST /api/webhook-jobs`
- `GET /api/webhook-jobs/:id`
- `POST /api/webhook-jobs/:id/retry`
- `GET /api/webhook-jobs/config`

状态：`pending`、`running`、`copying`、`checking`、`notifying_callback`、`calling_curl_url`、`success`、`failed`。

安全规则：

- rclone 使用 `exec.CommandContext`，不走 shell。
- path 拒绝空值、`..`、反斜杠、NUL 字节。
- 本地路径限制在 `RCLONE_MANAGER_WEBHOOK_LOCAL_BASE_DIR` 内，并拒绝已存在 symlink 路径组件。
- `callback_url` / `curl_url` 建议配置域名白名单。

---

## 云盘挂载说明

### 运行条件

云盘挂载基于 `rclone mount` + FUSE，需要满足：

1. 容器映射 `/dev/fuse`
2. 容器增加 `SYS_ADMIN` 能力
3. 容器增加 `apparmor:unconfined`
4. 如果希望宿主机直接看到挂载结果，请把你自己的宿主机目录 bind 到容器，并启用 `rshared` 传播

推荐 compose 片段：

```yaml
devices:
  - /dev/fuse:/dev/fuse
cap_add:
  - SYS_ADMIN
security_opt:
  - apparmor:unconfined
volumes:
  - type: bind
    source: /你的宿主机目录
    target: /你准备在容器里使用的挂载目录
    bind:
      propagation: rshared
```

如果你想限制所有挂载只能落在某个目录下，可以额外设置：

```env
RCLONE_MANAGER_MOUNT_ROOT=/data/cloud-mount
```

不设置则不限制，页面里直接填写完整挂载路径即可。

### 使用方式

1. 打开左侧 **云盘挂载** 页面
2. 新建挂载，选择远程盘符和远程路径
3. 设置本地挂载目录（现在需要你自己填写完整容器内路径）
4. 点击 **挂载**
5. 挂载成功后，可在 **文件浏览器** 或任务配置里把它当成本地目录使用

### 典型场景

- `alist:/影视` 挂载到 `/data/cloud-mount/movies`
- 然后创建任务：`/data/cloud-mount/movies` → 其它本地目录 / 其它远程盘
- 或直接在文件浏览器里浏览挂载后的云盘内容

---

## OpenList 配置说明

### 启用 OpenList 刷新

1. 编辑任务，展开 **OpenList 刷新设置**
2. 打开 **启用 OpenList 刷新** 开关
3. 填写 **OpenList 地址**
4. 填写 **认证 Token**（从 OpenList 管理后台获取）
5. （可选）填写 **路径映射**

### 认证方式

OpenList API 通过 `Authorization` Header 进行认证：

```
Authorization: openlist-xxx...
```

Token 在任务级别配置，每个任务可使用不同的 OpenList 实例和 Token。

## 项目结构

```
zzmrclone-manager/
├── backend/                  # Go 后端
│   ├── cmd/server/           # 入口程序
│   ├── internal/
│   │   ├── api/              # HTTP 路由与处理器
│   │   ├── auth/             # 认证相关
│   │   ├── config/           # 环境配置
│   │   ├── logger/           # 文件日志
│   │   ├── mounts/           # 云盘挂载管理
│   │   ├── models/           # GORM 数据模型
│   │   ├── rclone/           # Rclone 执行器 + OpenList 刷新
│   │   ├── scheduler/        # 定时任务调度
│   │   ├── watcher/          # 目录监控
│   │   └── websocket/        # WebSocket 推送
│   ├── go.mod
│   └── Dockerfile
├── frontend/                 # React 前端
│   ├── src/
│   │   ├── pages/            # 页面组件（含 Mounts.js 云盘挂载页）
│   │   ├── components/       # 公共组件
│   │   ├── services/         # API 封装
│   │   └── hooks/            # 状态管理
│   ├── public/
│   ├── package.json
│   └── Dockerfile
├── docker-compose.yml        # Docker 编排
├── nginx.conf                # Nginx 反向代理
├── supervisord.conf          # 进程管理
└── README.md
```

---

## Makefile 命令

| 命令 | 说明 |
|------|------|
| `make up` | 后台启动服务 |
| `make logs` | 查看实时日志 |
| `make restart` | 重启服务 |
| `make status` | 查看容器状态 |
| `make reset-password` | 重置管理员密码 |

---

## API 接口

### 认证
- `POST /api/login` — 用户登录
- `POST /api/register` — 用户注册
- `POST /api/change-password` — 修改密码

### 任务管理
- `GET /api/tasks` — 获取任务列表
- `POST /api/tasks` — 创建任务
- `GET /api/tasks/:id` — 获取任务详情
- `PUT /api/tasks/:id` — 更新任务
- `DELETE /api/tasks/:id` — 删除任务
- `POST /api/tasks/:id/start` — 启动任务
- `POST /api/tasks/:id/stop` — 停止任务
- `POST /api/tasks/:id/dedupe` — 执行去重
- `GET /api/tasks/:id/logs` — 获取任务日志
- `GET /api/tasks/:id/status` — 获取任务状态

### 系统
- `GET /api/system/stats` — 系统统计
- `GET /api/system/rclone-stats` — Rclone 实时统计
- `POST /api/system/log-level` — 设置日志级别
- `GET /api/system/logs` — 获取系统日志
- `POST /api/system/logs/clean` — 清空日志

### 云盘挂载
- `GET /api/mounts/system` — 获取挂载运行环境信息
- `GET /api/mounts` — 获取挂载配置列表
- `POST /api/mounts` — 创建挂载配置
- `GET /api/mounts/:id` — 获取挂载配置详情
- `PUT /api/mounts/:id` — 更新挂载配置
- `DELETE /api/mounts/:id` — 删除挂载配置
- `POST /api/mounts/:id/start` — 执行挂载
- `POST /api/mounts/:id/stop` — 执行卸载

### 转移记录（需 Token）
- `GET /api/output-logs?token=xxx` — 获取结构化转移记录
- `DELETE /api/output-logs/:id?token=xxx` — 删除单条记录
- `DELETE /api/output-logs/clean?token=xxx` — 清空记录

### Token 管理
- `GET /api/token?token=xxx` — 获取 Token 信息
- `POST /api/token?token=xxx` — 更新 Token

---

## 更新日志

### v1.1.1 (2026-05-12)

- **新增** 云盘本地挂载页面，支持创建、挂载、卸载、编辑、删除挂载配置
- **新增** 基于 `rclone mount` 的后端挂载管理与开机自挂载能力
- **新增** FUSE 运行环境检测与 Docker 部署说明

### v1.0.2 (2026-05-11)

- **新增** Docker Hub 预构建镜像支持（amd64 + arm64），无需本地编译
- **新增** GitHub Actions Release 自动构建推送镜像
- **变更** 部署方式从"克隆→构建"改为"直接 pull 镜像启动"

### v1.1.0 (2026-05-09)

- **新增** 首次部署自动生成随机管理员密码，密码写入日志及 `initial-password.txt`
- **新增** `--reset-password` CLI 命令，支持在容器内一键重置管理员密码
- **安全** 移除登录页面默认账号密码提示文字

### v1.0.1 (2026-05-01)

- **新增** OpenList 目录自动刷新功能（支持路径映射和 API Token 认证）
- **新增** 任务配置中 OpenList 刷新开关、地址、认证 Token、路径映射字段
- **变更** 关闭 gin HTTP 请求日志输出，降低磁盘 IO

---

## 开源协议

MIT License
