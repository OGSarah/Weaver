DATABASE_URL ?= postgres://weaver:weaver_dev_password@localhost:5432/weaver?sslmode=disable

.PHONY: migrate-up migrate-down migrate-new

migrate-up:
	migrate -path ./migrations -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path ./migrations -database "$(DATABASE_URL)" down 1

migrate-new:
	migrate create -ext sql -dir migrations -seq $(NAME)