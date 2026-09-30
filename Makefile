.PHONY: build test compose-up compose-down compose-smoke

build:
	CGO_ENABLED=0 go build -trimpath -o bin/inboxd ./cmd/inboxd
	CGO_ENABLED=0 go build -trimpath -o bin/agent-inbox ./cmd/agent-inbox

test:
	go test ./...

compose-up:
	docker compose up -d --build

compose-down:
	docker compose down

compose-smoke:
	./scripts/compose-smoke.sh
