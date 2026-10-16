.PHONY: build test compose-up compose-down compose-smoke getting-started-check walkthrough

build:
	CGO_ENABLED=0 go build -trimpath -o bin/inboxd ./cmd/inboxd
	CGO_ENABLED=0 go build -trimpath -o bin/agent-inbox ./cmd/agent-inbox
	CGO_ENABLED=0 go build -trimpath -o bin/agent-inbox-mcp ./cmd/agent-inbox-mcp

test:
	go test ./...

compose-up:
	docker compose up -d --build

compose-down:
	docker compose down

compose-smoke:
	./scripts/compose-smoke.sh

getting-started-check:
	./scripts/getting-started-check.sh

walkthrough:
	./scripts/walkthrough.sh
