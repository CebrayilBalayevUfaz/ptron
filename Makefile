BINARY ?= puppetron
NOVNC_VERSION ?= v1.5.0

.PHONY: build linux run novnc tidy test clean

build:
	go build -o $(BINARY) .

linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BINARY)-linux-amd64 .
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BINARY)-linux-arm64 .

tidy:
	go mod tidy

test:
	go test ./...

novnc:
	test -d novnc || git clone --depth 1 --branch $(NOVNC_VERSION) https://github.com/novnc/noVNC novnc

run: build novnc
	./$(BINARY) -listen :8080

clean:
	rm -f $(BINARY) $(BINARY)-linux-*
