MODULES  := rpc
FUZZTIME ?= 30s

.PHONY: all check test vet lint fmt-check tidy-check fuzz

all: check

check: fmt-check tidy-check vet lint test

test:
	@for m in $(MODULES); do echo "==> test $$m"; (cd $$m && go test -race -count=1 ./...) || exit 1; done

vet:
	@for m in $(MODULES); do echo "==> vet $$m"; (cd $$m && go vet ./...) || exit 1; done

lint:
	@for m in $(MODULES); do echo "==> lint $$m"; (cd $$m && golangci-lint run ./...) || exit 1; done

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

tidy-check:
	@for m in $(MODULES); do echo "==> tidy $$m"; (cd $$m && go mod tidy -diff) || exit 1; done

fuzz:
	@./scripts/fuzz.sh $(FUZZTIME) $(MODULES)
