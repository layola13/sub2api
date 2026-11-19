.PHONY: build run clean

build:
	cd backend && CGO_ENABLED=0 go build -o bin/mailbox ./cmd/mailbox

run: build
	cd backend && ./bin/mailbox

clean:
	rm -f backend/bin/mailbox
