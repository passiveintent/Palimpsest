.PHONY: tidy test race bench lint golden kafka-integration deps-check

tidy:
	go mod tidy

test:
	go test ./...

deps-check:
	# ADR-007: pkg/* stays stdlib + xxhash only. zstd (klauspost/compress),
	# franz-go, etc. must never appear in pkg/*'s dependency graph — they
	# belong in cmd/, internal/, otel/, or a leaf package like zstdcodec/.
	@bad=$$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./pkg/... | grep -v '^$$' | grep -v '^github.com/cespare/xxhash/v2$$' | grep -v '^github.com/passiveintent/Palimpsest/pkg/'); \
	if [ -n "$$bad" ]; then \
		echo "pkg/* imports something beyond stdlib+xxhash (ADR-007):"; echo "$$bad"; exit 1; \
	fi
	@echo "deps-check OK: pkg/* is stdlib + xxhash only"

race:
	@if [ "$(GOOS)" = "windows" ] || [ "$$(go env GOOS)" = "windows" ]; then \
		echo "⚠️  Skipping -race on Windows (requires cgo + MinGW GCC, not available by default here); run it in CI or a Linux/WSL/Docker shell instead"; \
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

kafka-integration:
	# internal/adapters/kafka's build-tag=integration suite against a real,
	# single-node Kafka (see demo/docker-compose.yml's "kafka" profile and
	# demo/README.md's Kafka section) rather than the in-memory fake broker
	# the normal `make test` already covers.
	cd demo && docker compose --profile kafka up -d kafka
	go test -tags integration ./internal/adapters/kafka/... -run TestIntegration -v
	cd demo && docker compose --profile kafka down
