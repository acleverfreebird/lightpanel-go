BINARY_NAME=lightpanel
BUILD_DIR=dist

.PHONY: all build-linux build-arm64 test vet release clean
all: build-linux build-arm64

build-linux:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 .

build-arm64:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 .

release: all
	cd $(BUILD_DIR) && sha256sum $(BINARY_NAME)-linux-amd64 > $(BINARY_NAME)-linux-amd64.sha256
	cd $(BUILD_DIR) && sha256sum $(BINARY_NAME)-linux-arm64 > $(BINARY_NAME)-linux-arm64.sha256

test:
	go test -race ./...

vet:
	go vet ./...

clean:
	rm -f $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64
