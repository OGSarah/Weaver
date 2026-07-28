DATABASE_URL ?= postgres://weaver:weaver_dev_password@localhost:5432/weaver?sslmode=disable

# Tests that need a database point at their own, so a run never competes with the
# stack in Docker Compose for the same queue. See `make test-db`.
TEST_DATABASE_URL ?= postgres://weaver:weaver_dev_password@localhost:5432/weaver_test?sslmode=disable

.PHONY: migrate-up migrate-down migrate-new test test-unit test-db lint

migrate-up:
	migrate -path ./migrations -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path ./migrations -database "$(DATABASE_URL)" down 1

migrate-new:
	migrate create -ext sql -dir migrations -seq $(NAME)

# Create the test database and apply every migration to it. Run once, and again
# after adding a migration. Postgres comes from Compose: `docker compose up -d postgres`.
test-db:
	docker exec -i weaver-postgres psql -U weaver -d postgres \
		-c "DROP DATABASE IF EXISTS weaver_test;" -c "CREATE DATABASE weaver_test;"
	for f in migrations/*.up.sql; do \
		docker exec -i weaver-postgres psql -q -U weaver -d weaver_test -v ON_ERROR_STOP=1 < $$f; \
	done

# The whole suite, including the tests that need Postgres.
test:
	DATABASE_URL="$(TEST_DATABASE_URL)" go test -race ./...

# Just the tests that need nothing: the database-backed ones skip themselves.
test-unit:
	go test -race ./...

lint:
	gofmt -l . | grep -v '^web/node_modules/' | (! grep .) || (echo "run gofmt -w on the files above"; exit 1)
	go vet ./...
	cd web && npm run lint -- --max-warnings=0
