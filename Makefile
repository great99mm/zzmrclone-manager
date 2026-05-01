.PHONY: build up down logs clean dev

build:
	docker-compose build

up:
	docker-compose up -d

down:
	docker-compose down

logs:
	docker-compose logs -f

clean:
	docker-compose down -v
	docker system prune -f

dev:
	docker-compose up

restart:
	docker-compose restart

status:
	docker-compose ps
