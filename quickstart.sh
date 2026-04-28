#!/bin/bash
set -e

echo "=========================================="
echo "   Rclone Manager - 快速启动脚本"
echo "=========================================="

# Check rclone config
if [ ! -f "$HOME/.config/rclone/rclone.conf" ]; then
    echo "⚠️  未检测到 rclone 配置文件"
    echo "请先运行: rclone config"
    exit 1
fi

echo "✅ rclone 配置已检测到"

# Check docker
if ! command -v docker &> /dev/null; then
    echo "❌ Docker 未安装"
    exit 1
fi

if ! command -v docker-compose &> /dev/null; then
    echo "❌ Docker Compose 未安装"
    exit 1
fi

echo "✅ Docker 环境已就绪"

# Create directories
mkdir -p data logs

# Build and start
echo ""
echo "🚀 正在构建并启动服务..."
docker-compose up -d --build

echo ""
echo "✅ 服务已启动！"
echo ""
echo "📱 管理界面: http://localhost:7071"
echo "🔌 API 地址: http://localhost:7070"
echo "📊 Rclone RC: http://localhost:5572"
echo ""
echo "默认账号:"
echo "  用户名: admin"
echo "  密码: admin123"
echo ""
echo "查看日志: docker-compose logs -f"
echo "停止服务: docker-compose down"
