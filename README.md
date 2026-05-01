# ZZMRClone Manager

一款基于 Web 的 Rclone 自动化管理工具，支持任务调度、目录监控、实时日志、结构化转移记录，以及 **OpenList 目录自动刷新**。提供可视化界面和持久化数据库，让 Rclone 文件传输管理更加高效。

---

## 功能特性

### 核心功能

- **任务管理** — 创建、编辑、启停、删除 Rclone 传输任务
- **目录监控** — 实时监听源目录变化，文件新增后自动触发传输
- **定时执行** — 支持按固定间隔（分钟）自动执行任务
- **实时日志** — WebSocket 推送任务执行日志，支持倒序查看和关键字高亮
- **结构化转移记录** — 每条文件传输自动生成持久化记录，支持分页查询和筛选

### OpenList 集成（v1.0 新增）

- **自动目录刷新** — 任务转移成功后，自动调用 OpenList API 刷新目标目录缓存
- **路径映射** — 支持配置路径映射关系，解决 OpenList 挂载路径与 rclone 目标路径不一致的问题
- **刷新状态追踪** — 转移记录中展示每条文件的 OpenList 刷新结果（成功/失败）

### 系统特性

- **去重操作** — 转移完成后自动执行 `rclone dedupe newest`
- **Token 保护** — API 支持 Token 鉴权，防止未授权访问
- **轻量日志** — 关闭 gin HTTP 请求日志，降低磁盘 IO，仅保留业务日志
- **Docker 部署** — 一键启动，开箱即用

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

### Docker 部署（推荐）

```bash
git clone https://github.com/great99mm/zzmrclone-manager.git
cd zzmrclone-manager

# 启动服务
docker-compose up -d
```

访问 `http://ip:7071`，默认账号 `admin` / `admin123`。

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `RCLONE_MANAGER_DATA_DIR` | `/app/data` | 数据目录（SQLite 数据库） |
| `RCLONE_MANAGER_LOG_DIR` | `/app/logs` | 日志文件目录 |
| `RCLONE_MANAGER_PORT` | `7070` | HTTP 服务端口 |
| `RCLONE_CONFIG` | `/root/.config/rclone/rclone.conf` | Rclone 配置文件路径 |
| `RCLONE_MANAGER_API_TOKEN` | `""` | API Token（空表示不启用） |

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

### 路径映射配置

当 rclone 目标路径与 OpenList 实际挂载路径不一致时，可通过路径映射修正刷新目录。

**配置格式：** JSON 对象，键为 rclone 路径前缀，值为 OpenList 对应路径。

| 场景 | 映射配置 | 说明 |
|------|---------|------|
| rclone `op:s1` → OpenList `/s2` | `{"op:s1": "/s2"}` | 刷新 `/s2` 而非 `/s1` |
| rclone `op:gdrive` → OpenList `/mount/gd` | `{"op:gdrive": "/mount/gd"}` | 刷新 `/mount/gd` |

**示例流程：**

文件从 `/mnt/a.txt` 转移到 `op:/s1/a.txt`，配置映射 `{"op:s1": "/s2"}`：

1. 程序提取 rclone 目标目录：`/s1`
2. 应用路径映射：`/s1` → `/s2`
3. 调用 OpenList API：`POST https://fox.oplist.org/364155732e0/api/fs/list`
4. 请求体：`{"path": "/s2", "refresh": true, "page": 1, "per_page": 0}`

---

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
│   │   ├── models/           # GORM 数据模型
│   │   ├── rclone/           # Rclone 执行器 + OpenList 刷新
│   │   ├── scheduler/        # 定时任务调度
│   │   ├── watcher/          # 目录监控
│   │   └── websocket/        # WebSocket 推送
│   ├── go.mod
│   └── Dockerfile
├── frontend/                 # React 前端
│   ├── src/
│   │   ├── pages/            # 页面组件
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

### 转移记录（需 Token）
- `GET /api/output-logs?token=xxx` — 获取结构化转移记录
- `DELETE /api/output-logs/:id?token=xxx` — 删除单条记录
- `DELETE /api/output-logs/clean?token=xxx` — 清空记录

### Token 管理
- `GET /api/token?token=xxx` — 获取 Token 信息
- `POST /api/token?token=xxx` — 更新 Token

---

## 更新日志

### v1.0.1 (2026-05-01)

- **新增** OpenList 目录自动刷新功能（支持路径映射和 API Token 认证）
- **新增** 任务配置中 OpenList 刷新开关、地址、认证 Token、路径映射字段
- **新增** 转移记录中展示 OpenList 刷新状态
- **变更** 关闭 gin HTTP 请求日志输出，降低磁盘 IO
- **优化** 前后端字段校验与 URL 规范化处理

---

## 开源协议

MIT License
