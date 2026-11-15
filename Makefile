.PHONY: build test test-pg race demo ui-smoke

build:
	go build -o bin/bankcmp ./cmd/bankcmp
	go build -o bin/seedtest ./cmd/seedtest

test:
	go test ./...

test-pg:
	@test -n "$$BANKCMP_TEST_PG" || (echo "set BANKCMP_TEST_PG to a PostgreSQL admin DSN" && exit 1)
	go test ./...

race:
	go test -race ./internal/engine ./internal/plan

demo:
	./scripts/demo.sh

ui-smoke:
	python3 scripts/ui_smoke.py $${BASE:-http://127.0.0.1:8080}
