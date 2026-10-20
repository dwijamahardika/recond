.PHONY: build test test-race lint fmt vet bench bench-e2e fuzz demo serve docker clean

BIN := bin

build:
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/recond ./cmd/recond
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/gen ./cmd/gen

test:
	go test -count=1 -cover ./...

test-race:
	CGO_ENABLED=1 go test -race -count=1 ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

## Micro-benchmarks: amount parsing, settlement adapters, state machine, matcher.
bench:
	go test -run '^$$' -bench . -benchmem ./internal/domain ./internal/ingest ./internal/match

## End-to-end: generate, ingest, run, report, verify, score. SIZES="100000 1000000"
bench-e2e:
	scripts/bench.sh $(SIZES)

## A short fuzzing pass over every parser (FUZZTIME per target).
FUZZTIME ?= 20s
fuzz:
	for t in FuzzParseAmount FuzzFormatRoundTrip; do go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) ./internal/domain || exit 1; done
	for t in FuzzPSPA FuzzBankB FuzzLedger FuzzDecodeEvent; do go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) ./internal/ingest || exit 1; done

## Generate a faulty world, reconcile it, score it against ground truth, work an exception.
demo:
	scripts/demo.sh

serve:
	go run ./cmd/recond serve

docker:
	docker build -t recond:dev .

clean:
	rm -rf $(BIN) demo-out bench-out reports cover.out
