# Rclone Manager - 项目概览

## 架构设计

```
┌─────────────────────────────────────────────────────────────┐
│                        Docker Compose                        │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐   │
│  │   Frontend   │    │   Backend    │    │   Rclone     │   │
│  │   (React)    │◄──►│    (Go)      │◄──►│   Daemon     │   │
│  │   :7071      │    │   :7070      │    │   :5572      │   │
│  └──────────────┘    └──────────────┘    └──────────────┘   │
│         │                   │                                │
│         │            ┌────┴────┐                          │
│         │            │  SQLite │                          │
│         │            │  :data   │                          │
│         │            └────┬────┘                          │
│         │                 │                                 │
│    WebSocket ◄────────────┘                                │
│    (实时日志)                                               │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

## 功能对照表

| 原脚本功能 | 实现方式 | 文件位置 |
|-----------|---------|---------|
| 交互式配置 (setup_config) | Web 表单 + REST API | `frontend/src/pages/TaskForm.js` |
| rclone move 传输 | Go exec.Command | `backend/internal/rclone/rclone.go` |
| rclone dedupe 去重 | 自动/手动触发 | `backend/internal/rclone/rclone.go` |
| systemd path 目录监控 | fsnotify | `backend/internal/watcher/watcher.go` |
| systemd timer 定时任务 | robfig/cron | `backend/internal/scheduler/scheduler.go` |
| rc status 状态看板 | Dashboard 页面 | `frontend/src/pages/Dashboard.js` |
| rc vv/v 日志级别切换 | API + rclone rc | `frontend/src/pages/Settings.js` |
| rc logs/debug 日志查看 | WebSocket 实时推送 | `frontend/src/pages/TaskDetail.js` |
| rc clean 日志清理 | API 端点 | `frontend/src/pages/Logs.js` |
| 多任务支持 | SQLite + CRUD | `backend/internal/api/router.go` |
| IPv6 绑定 | 任务配置参数 | `frontend/src/pages/TaskForm.js` |

## 多任务设计

每个任务独立配置：
- 不同的源目录 (source_dir)
- 不同的远程盘符 (remote_name)
- 不同的远程目录 (remote_dir)
- 不同的 rclone 配置路径 (rclone_config)
- 独立的传输参数 (transfers, checkers, chunk_size 等)
- 独立的监控/定时设置

## 数据流

1. **创建任务** → 前端表单 → POST /api/tasks → SQLite 存储
2. **目录监控** → fsnotify 检测变化 → 10秒防抖 → 触发 rclone move
3. **定时执行** → cron 调度 → 检查是否运行中 → 触发 rclone move
4. **实时日志** → rclone 输出 → WebSocket → 前端展示
5. **任务状态** → 轮询 + WebSocket 事件 → Dashboard 更新

## 端口映射

| 服务 | 容器端口 | 宿主机端口 | 说明 |
|------|---------|-----------|------|
| Frontend | 80 | 7071 | Web UI |
| Backend | 7070 | 7070 | REST API |
| Rclone Daemon | 5572 | 5572 | RC API |

## 部署步骤

```bash
# 1. 确保 rclone 配置存在
ls ~/.config/rclone/rclone.conf

# 2. 克隆/解压项目
cd rclone-manager

# 3. 启动服务
docker-compose up -d

# 4. 访问管理界面
open http://localhost:7071

# 5. 默认登录
# 用户名: admin
# 密码: admin123
```

## 技术栈

- **后端**: Go 1.22 + Gin + GORM + SQLite
- **前端**: React 18 + Tailwind CSS + WebSocket + Zustand
- **基础设施**: Docker Compose + rclone/rclone:latest
- **监控**: fsnotify (目录) + cron (定时)
- **通信**: REST API + WebSocket
