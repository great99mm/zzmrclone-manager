.PHONY: build up down logs clean dev

build:
	docker build -t dedehao/zzmrclone-manager:latest .

up:
	docker build -t dedehao/zzmrclone-manager:latest .
	docker compose up -d --force-recreate

down:
	docker compose down

logs:
	docker compose logs -f

clean:
	docker compose down -v
	docker system prune -f

dev:
	docker build -t dedehao/zzmrclone-manager:latest .
	docker compose up

restart:
	docker compose restart

status:
	docker compose ps

reset-password:
	docker exec rclone-manager /app/server --reset-password
