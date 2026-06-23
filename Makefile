# Layer: monorepo root — top-level Makefile.
# Drives building, testing, linting, and code generation for all components.
# Each target has a comment explaining what it does and which layer it belongs to.

.PHONY: all build test lint proto ebpf rust-build clean help

# Default: build everything.
all: build

## build: Compile all Go binaries into ./bin/
build:
	@echo "==> Building Go services..."
	go build -o bin/api        ./cmd/api
	go build -o bin/orchestrator ./cmd/orchestrator
	go build -o bin/host-agent ./cmd/host-agent
	@echo "==> Building Rust vm-agent..."
	cd vm-agent && cargo build --release
	@cp vm-agent/target/release/vm-agent bin/vm-agent 2>/dev/null || true
	@echo "==> Building TypeScript SDK..."
	cd sdk/typescript && npm run build
	@echo "Build complete. Binaries in ./bin/"

## test: Run all Go, Rust, and TypeScript tests.
test: test-go test-rust test-ts

## test-go: Run Go unit tests (no KVM or Linux required).
test-go:
	@echo "==> Running Go tests..."
	go test ./internal/... ./cmd/orchestrator/... -v -count=1

## test-rust: Run Rust vm-agent tests.
test-rust:
	@echo "==> Running Rust tests..."
	cd vm-agent && cargo test

## test-ts: Run TypeScript SDK tests.
test-ts:
	@echo "==> Running TypeScript tests..."
	cd sdk/typescript && npm run build && node src/client.test.js 2>/dev/null || node dist/client.test.js

## lint: Run Go vet and Rust clippy.
lint:
	@echo "==> Go vet..."
	go vet ./...
	@echo "==> Rust clippy..."
	cd vm-agent && cargo clippy -- -D warnings

## proto: Regenerate gRPC Go stubs from proto/sandock.proto.
## Requires: protoc, protoc-gen-go, protoc-gen-go-grpc
## Install:  go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
##           go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
proto:
	@echo "==> Generating gRPC stubs..."
	mkdir -p proto/gen
	protoc \
	  --go_out=proto/gen --go_opt=paths=source_relative \
	  --go-grpc_out=proto/gen --go-grpc_opt=paths=source_relative \
	  --proto_path=proto \
	  proto/sandock.proto
	@echo "Stubs written to proto/gen/"

## ebpf: Compile the eBPF C program to an object file (Linux + clang required).
ebpf:
	@echo "==> Compiling eBPF egress filter..."
	clang -O2 -target bpf -D__TARGET_ARCH_x86 \
	  -I/usr/include/bpf \
	  -I/usr/include/linux \
	  -c ebpf/programs/egress_filter.c \
	  -o ebpf/programs/egress_filter.o
	@echo "eBPF object written to ebpf/programs/egress_filter.o"

## rust-build: Build the Rust vm-agent only.
rust-build:
	cd vm-agent && cargo build --release

## clean: Remove build artifacts.
clean:
	@echo "==> Cleaning..."
	rm -rf bin/
	cd vm-agent && cargo clean
	cd sdk/typescript && rm -rf dist/ node_modules/

## help: Print this help.
help:
	@echo "sandock Makefile targets:"
	@grep -E '^##' Makefile | sed 's/## /  /'
