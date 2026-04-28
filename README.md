# Rclone Manager

基于 Web 的 Rclone 自动化管理工具，支持多任务配置、目录监控、定时执行和实时日志查看。

## 功能特性

- **多任务管理**：每个任务可配置不同的源目录、远程盘符和传输参数
- **目录监控**：源目录有文件变化时自动触发传输（10秒防抖）
- **定时执行**：按固定间隔自动执行传输任务
- **自动去重**：传输完成后自动执行 `rclone dedupe newest`
- **实时日志**：WebSocket 推送实时输出，支持日志级别切换
- **Web 端配置**：所有参数通过前端界面设置，无需修改配置文件

## 快速开始

### 1. 准备 rclone 配置

确保宿主机已有 rclone 配置文件：
```bash
rclone config
# 配置文件通常位于 ~/.config/rclone/rclone.conf
```

### 2. 启动服务

```bash
git clone https://github.com/great99mm/zzmrclone-manager.git
cd zzmrclone-manager
docker compose up -d --build
```

### 3. 访问管理界面

- 前端: http://localhost:7071

### 4. 默认登录

- 用户名: `admin`
- 密码: `admin123`

## 配置说明

### Docker Compose 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `RCLONE_MANAGER_DATA_DIR` | /app/data | 数据存储目录 |
| `RCLONE_MANAGER_LOG_DIR` | /app/logs | 日志存储目录 |
| `RCLONE_MANAGER_PORT` | 7070 | 后端服务端口 |
| `RCLONE_CONFIG` | /root/.config/rclone/rclone.conf | rclone 配置文件路径 |

### 任务参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| transfers | 16 | 并发传输数 |
| checkers | 32 | 并发检查数 |
| min_age | 10s | 最小文件年龄 |
| drive_chunk_size | 256M | 上传块大小 |
| buffer_size | 512M | 缓冲区大小 |
| retries | 3 | 重试次数 |
| bind_ip | - | 绑定 IP（支持 IPv6） |

## 目录结构

```
rclone-manager/
├── docker-compose.yml
├── backend/
│   ├── Dockerfile
│   ├── go.mod
│   └── cmd/server/
│       └── main.go
├── frontend/
│   ├── Dockerfile
│   ├── package.json
│   └── src/
│       ├── App.js
│       ├── index.js
│       ├── pages/
│       │   ├── Dashboard.js
│       │   ├── Tasks.js
│       │   ├── TaskForm.js
│       │   ├── TaskDetail.js
│       │   ├── Logs.js
│       │   ├── Settings.js
│       │   └── Login.js
│       ├── components/
│       │   └── Sidebar.js
│       ├── services/
│       │   └── api.js
│       └── hooks/
│           └── useAuthStore.js
└── data/          # 数据持久化目录
└── logs/          # 日志目录
```

## 技术栈

- **Backend**: Go + Gin + GORM + SQLite
- **Frontend**: React + Tailwind CSS + WebSocket
- **Infrastructure**: Docker Compose + rclone daemon

## License

MIT
