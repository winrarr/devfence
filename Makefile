.PHONY: build check fmt integration-vm test vet

build:
	go build -o devfence ./cmd/devfence

fmt:
	test -z "$$(gofmt -l $$(rg --files -g '*.go'))"
	bash -n scripts/integration-vm.sh

test:
	go test ./...

vet:
	go vet ./...

check: fmt vet test build

integration-vm: build
	bash scripts/integration-vm.sh
