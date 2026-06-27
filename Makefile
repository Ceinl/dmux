.PHONY: build test race vet lint run clean

BIN := dmux
PKG := ./...

build:
	go build -o $(BIN) ./cmd/dmux

test:
	go test $(PKG)

race:
	go test -race $(PKG)

vet:
	go vet $(PKG)

lint:
	golangci-lint run

run: build
	./$(BIN) serve

clean:
	rm -f $(BIN)
	go clean
