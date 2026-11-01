MODULES  := . rpc grpcbridge
# An execution count, not a duration: when a duration runs out, Go's fuzz
# coordinator can fail the run with a spurious "context deadline exceeded".
FUZZTIME ?= 1000000x
# Targets that start a server and a client per input are about ten times
# slower; they get fewer runs.
FUZZ_OVERRIDES ?= FuzzServerStream=100000x FuzzClientResponse=100000x
# Flags for go test besides -count=1; CI also runs the tests with -cpu=1.
TESTFLAGS ?= -race
# Network profile of testbed-shell and bench-run, from
# testbed/scenarios/profiles.yaml or points.yaml.
PROFILE ?= none
# Transport, load profile (testbed/scenarios/load.yaml) and seed of
# bench-run, and more flags of bench run, such as -strict.
TRANSPORT ?= quic
LOAD      ?= small
SEED      ?= 1
RUNFLAGS  ?=
# Matrix of bench-series and more flags of bench series, such as -out DIR
# to resume a series.
MATRIX      ?= testbed/scenarios/pilot.yaml
SERIESFLAGS ?=
# CPUs of the orchestrator of bench-run and bench-series, the E-cores of the
# reference machine; empty leaves it unpinned.
ORCH_CPUS ?= 8-11
# Series of plots and the directory of its tables and figures; with ANALYSIS
# alone, plots works from the runs.csv and calls.parquet already there.
SERIES   ?=
ANALYSIS ?= $(if $(SERIES),$(SERIES)/analysis)
# The Python of the analysis, with the packages of requirements.txt.
VENV      := .venv
PLOTS_RUN := PYTHONPATH=analysis/plots $(VENV)/bin/python

PROTOC_GEN_GO      := $(CURDIR)/bin/protoc-gen-go
PROTOC_GEN_GO_GRPC := $(CURDIR)/bin/protoc-gen-go-grpc
PROTOC_GEN_RPCQUIC := $(CURDIR)/bin/protoc-gen-rpcquic
PROTOC_GEN_CONNECT := $(CURDIR)/bin/protoc-gen-connect-go
GENERATORS         := $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC) $(PROTOC_GEN_RPCQUIC) $(PROTOC_GEN_CONNECT)
# Relative to rpc/.
RPC_PROTOS := internal/frame/framepb/frame.proto
# Relative to proto/; the code goes to internal/gen of the root module.
ROOT_PROTOS := difftest/v1/difftest.proto bench/v1/bench.proto
ROOT_MODULE := github.com/VladimirKhmelev/quic-rpc-go
# Test input of protoc-gen-rpcquic, kept as a descriptor set so that its
# tests run without protoc.
PLUGIN_TESTDATA := rpc/cmd/protoc-gen-rpcquic/testdata
PLUGIN_PROTOS   := echo/v1/echo.proto

.PHONY: all check test vet lint fmt-check tidy-check fuzz proto proto-check proto-check-go proto-check-descriptors testbed-shell testbed-check host-setup host-restore bench-run bench-series bench-pilot plots plots-test

all: check

check: fmt-check tidy-check proto-check vet lint test

test:
	@for m in $(MODULES); do echo "==> test $$m"; (cd $$m && go test -count=1 $(TESTFLAGS) ./...) || exit 1; done

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

# A shell inside the testbed; the testbed is gone when the shell exits.
testbed-shell:
	go run ./testbed/cmd/testbed shell -profile $(PROFILE)

# The self-check of every profile and point (NET-4), about 11 minutes;
# needs iperf3.
testbed-check:
	go run ./testbed/cmd/testbed check -all -v

# One run of the benchmark in the testbed, into out/ (see data/schema.md).
# go build stamps the git commit into the binary for meta.json.
bench-run:
	go build -o bin/bench ./bench/cmd/bench
	$(if $(ORCH_CPUS),taskset -c $(ORCH_CPUS)) bin/bench run -transport $(TRANSPORT) -profile $(PROFILE) -load $(LOAD) -seed $(SEED) $(RUNFLAGS)

# A series of runs into out/ (see data/schema.md): every run of MATRIX,
# after a self-check of the testbed against its networks.
bench-series:
	go build -o bin/bench ./bench/cmd/bench
	go build -o bin/testbed ./testbed/cmd/testbed
	$(if $(ORCH_CPUS),taskset -c $(ORCH_CPUS)) bin/bench series -matrix $(MATRIX) $(SERIESFLAGS)

# The pilot series (phase 2), on a machine tuned with host-setup.
bench-pilot: MATRIX = testbed/scenarios/pilot.yaml
bench-pilot: override SERIESFLAGS += -strict
bench-pilot: bench-series

# The tables, figures and report.md of a series (ANL-1, see data/schema.md):
# runs.csv and calls.parquet from the runs of SERIES, then the statistics
# and the figures from them, all into ANALYSIS.
plots: $(VENV)/.installed
	@test -n "$(ANALYSIS)" || { echo "plots needs SERIES=DIR or ANALYSIS=DIR"; exit 2; }
	$(if $(SERIES),$(PLOTS_RUN) -m qrpcplots ingest $(SERIES) -o $(ANALYSIS))
	$(PLOTS_RUN) -m qrpcplots report $(ANALYSIS)

plots-test: $(VENV)/.installed
	$(PLOTS_RUN) -m pytest -q analysis/plots

$(VENV)/.installed: analysis/plots/requirements.txt
	python3 -m venv $(VENV)
	$(VENV)/bin/pip install -q -r analysis/plots/requirements.txt
	touch $@

# Tune the machine for a series (NET-5) and put it back; run with sudo.
host-setup:
	testbed/env/host-setup.sh

host-restore:
	testbed/env/host-setup.sh --restore

# Generators: protoc-gen-go from the protobuf version pinned in rpc/go.mod,
# protoc-gen-go-grpc and protoc-gen-connect-go from tool directives of go.mod, protoc-gen-rpcquic
# from this repository.
$(PROTOC_GEN_GO): rpc/go.mod rpc/go.sum
	cd rpc && go build -o $@ google.golang.org/protobuf/cmd/protoc-gen-go

$(PROTOC_GEN_GO_GRPC): go.mod go.sum
	go build -o $@ google.golang.org/grpc/cmd/protoc-gen-go-grpc

$(PROTOC_GEN_CONNECT): go.mod go.sum
	go build -o $@ connectrpc.com/connect/v2/cmd/protoc-gen-connect-go

$(PROTOC_GEN_RPCQUIC): rpc/go.mod rpc/go.sum $(wildcard rpc/cmd/protoc-gen-rpcquic/*.go)
	cd rpc && go build -o $@ ./cmd/protoc-gen-rpcquic

# protoc_rpc and protoc_root write the generated code under directory $(1).
protoc_rpc = cd rpc && protoc --plugin=protoc-gen-go=$(PROTOC_GEN_GO) --go_out=$(1) --go_opt=paths=source_relative $(RPC_PROTOS)
protoc_root = protoc -I proto \
	--plugin=protoc-gen-go=$(PROTOC_GEN_GO) --go_out=$(1) --go_opt=module=$(ROOT_MODULE) \
	--plugin=protoc-gen-go-grpc=$(PROTOC_GEN_GO_GRPC) --go-grpc_out=$(1) --go-grpc_opt=module=$(ROOT_MODULE) \
	--plugin=protoc-gen-rpcquic=$(PROTOC_GEN_RPCQUIC) --rpcquic_out=$(1) --rpcquic_opt=module=$(ROOT_MODULE) \
	--plugin=protoc-gen-connect-go=$(PROTOC_GEN_CONNECT) --connect-go_out=$(1) --connect-go_opt=module=$(ROOT_MODULE) \
	$(ROOT_PROTOS)

proto: $(GENERATORS)
	$(call protoc_rpc,.)
	$(call protoc_root,.)
	cd $(PLUGIN_TESTDATA) && protoc --include_source_info --descriptor_set_out=echo.binpb $(PLUGIN_PROTOS)

proto-check: proto-check-go proto-check-descriptors

proto-check-go: $(GENERATORS)
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && mkdir "$$tmp/rpc" "$$tmp/root" && \
	($(call protoc_rpc,$$tmp/rpc)) && $(call protoc_root,$$tmp/root) && \
	for pair in "$$tmp/rpc:rpc" "$$tmp/root:."; do \
		gen=$${pair%%:*}; dst=$${pair#*:}; \
		for f in $$(cd "$$gen" && find . -name '*.go'); do \
			diff -u "$$dst/$$f" "$$gen/$$f" || { echo "generated code is stale, run make proto"; exit 1; }; \
		done; \
	done

proto-check-descriptors:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	cd $(PLUGIN_TESTDATA) && protoc --include_source_info --descriptor_set_out=$$tmp/echo.binpb $(PLUGIN_PROTOS) && \
	cmp -s echo.binpb $$tmp/echo.binpb || { echo "$(PLUGIN_TESTDATA)/echo.binpb is stale, run make proto"; exit 1; }
