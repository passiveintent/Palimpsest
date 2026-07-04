.PHONY: tidy test race bench lint golden

tidy:
	go mod tidy

test:
	go test ./...

race:
	@if [ "$(GOOS)" = "windows" ] || [ "$$(go env GOOS)" = "windows" ]; then \
		echo "⚠️  Skipping -race on Windows (requires cgo + MinGW GCC; code has no goroutines)"; \
		go test ./...; \
	else \
		go test -race ./...; \
	fi

bench:
	go test -bench=. -benchmem -run='^$$' ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not found; falling back to go vet"; \
		go vet ./...; \
	fi

golden:
	cd oracle && python3 gen_golden.py --out ../testdata/golden
