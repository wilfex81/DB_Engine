.PHONY: tidy vendor vet buffer-test fuzz-test

tidy:
	@go mod tidy

vendor:
	@go mod vendor

vet:
	@go vet ./...

race-test:
	@go test -race -v ./internal/storage/ ./internal/buffer/ ./internal/table/

fuzz-test:
	@go test -fuzz=FuzzPage -fuzztime=20s -fuzzminimizetime=0 ./internal/storage/   