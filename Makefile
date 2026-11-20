.PHONY: tidy vendor vet buffer-test fuzz-test

tidy:
	@go mod tidy

vendor:
	@go mod vendor

vet:
	@go vet ./...

buffer-test:
	@go test -race -v ./internal/buffer/

fuzz-test:
	@go test -fuzz=FuzzPage -fuzztime=20s -fuzzminimizetime=0 ./internal/storage/   