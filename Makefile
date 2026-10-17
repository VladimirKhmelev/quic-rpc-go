MODULES  := rpc
# An execution count, not a duration: when a duration runs out, Go's fuzz
# coordinator can fail the run with a spurious "context deadline exceeded".
FUZZTIME ?= 1000000x
# Targets that start a server and a client per input are about ten times
# slower; they get fewer runs.
FUZZ_OVERRIDES ?= FuzzServerStream=100000x FuzzClientResponse=100000x

PROTOC_GEN_GO := $(CURDIR)/bin/protoc-gen-go
# Relative to rpc/.
RPC_PROTOS := internal/frame/framepb/frame.proto
# Test input of protoc-gen-rpcquic, kept as a descriptor set so that its
# tests run without protoc.
PLUGIN_TESTDATA := rpc/cmd/protoc-gen-rpcquic/testdata
PLUGIN_PROTOS   := echo/v1/echo.proto

.PHONY: all check test vet lint fmt-check tidy-check fuzz proto proto-check proto-check-go proto-check-descriptors

all: check

check: fmt-check tidy-check proto-check vet lint test

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
	@FUZZ_OVERRIDES="$(FUZZ_OVERRIDES)" ./scripts/fuzz.sh $(FUZZTIME) $(MODULES)

# protoc-gen-go is built from the protobuf version pinned in rpc/go.mod.
$(PROTOC_GEN_GO): rpc/go.mod rpc/go.sum
	cd rpc && go build -o $(PROTOC_GEN_GO) google.golang.org/protobuf/cmd/protoc-gen-go

proto: $(PROTOC_GEN_GO)
	cd rpc && protoc --plugin=protoc-gen-go=$(PROTOC_GEN_GO) --go_out=. --go_opt=paths=source_relative $(RPC_PROTOS)
	cd $(PLUGIN_TESTDATA) && protoc --include_source_info --descriptor_set_out=echo.binpb $(PLUGIN_PROTOS)

proto-check: proto-check-go proto-check-descriptors

proto-check-go: $(PROTOC_GEN_GO)
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	cd rpc && protoc --plugin=protoc-gen-go=$(PROTOC_GEN_GO) --go_out=$$tmp --go_opt=paths=source_relative $(RPC_PROTOS) && \
	for f in $$(cd $$tmp && find . -name '*.pb.go'); do \
		diff -u "$$f" "$$tmp/$$f" || { echo "generated code is stale, run make proto"; exit 1; }; \
	done

proto-check-descriptors:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	cd $(PLUGIN_TESTDATA) && protoc --include_source_info --descriptor_set_out=$$tmp/echo.binpb $(PLUGIN_PROTOS) && \
	cmp -s echo.binpb $$tmp/echo.binpb || { echo "$(PLUGIN_TESTDATA)/echo.binpb is stale, run make proto"; exit 1; }
