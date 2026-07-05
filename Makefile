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
	# hash/hkdf/dictroot/kdelta/quant/frame_residual.bin: BYTE-EXACT vs oracle.
	# recovery_case1.json / recovery_watermark.json: TOLERANCE-compared in Go
	# (BLAS backends differ in low-order bits; _canon rounds to 1e-9 for
	# stability — see docs/adr/ADR-001).
	cd oracle && python3 gen_golden.py --out ../testdata/golden
