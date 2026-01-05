.PHONY: test test-regtest test-regtest-custommsg test-regtest-grpc test-regtest-openai-serve lint tools gen fmt proto-deps devnet-up devnet-down devnet-status devnet-info

# Nix-first workflow:
# - All targets are meant to run inside the go-lcpd devShell (`nix develop`).
# - If you invoke `make` outside the devShell, this Makefile re-execs itself via
#   `nix develop -c make ...` so that tool versions are pinned by flake.lock.
.DEFAULT_GOAL := test

NIX_DEVELOP ?= nix develop
GOALS := $(if $(MAKECMDGOALS),$(MAKECMDGOALS),$(.DEFAULT_GOAL))

ifeq ($(strip $(IN_NIX_SHELL)),)
.PHONY: _nix $(GOALS)
$(GOALS): _nix
	@:

_nix:
	$(NIX_DEVELOP) -c $(MAKE) $(GOALS)

else

TOOLS_BIN := $(CURDIR)/.tools/bin

test:
	go test ./...

test-e2e:
	go test ./itest/e2e -count=1 -v

test-regtest:
	LCP_ITEST_REGTEST=1 go test ./itest/e2e -run Regtest -count=1 -v

test-regtest-custommsg:
	LCP_ITEST_REGTEST=1 go test ./itest/e2e -run Regtest_LNDCustomMessages -count=1 -v

test-regtest-grpc:
	LCP_ITEST_REGTEST=1 go test ./itest/e2e -run RequesterGRPC -count=1 -v

test-regtest-openai-serve:
	LCP_ITEST_REGTEST=1 go test ./itest/e2e -run Regtest_OpenAIServe -count=1 -v

test-e2e-regtest:
	LCP_ITEST_REGTEST=1 go test ./itest/e2e -run Regtest -count=1 -v

devnet-up:
	./scripts/devnet up

devnet-down:
	./scripts/devnet down

devnet-status:
	./scripts/devnet status

devnet-info:
	./scripts/devnet info

tools:
	@mkdir -p $(TOOLS_BIN)
	@GOBIN=$(TOOLS_BIN) go install github.com/NathanBaulch/protoc-gen-cobra@v1.2.1

gen: tools
	PATH="$(TOOLS_BIN):$$PATH" buf generate

lint: tools
	buf lint
	golangci-lint version
	golangci-lint run -c .golangci.yml ./...

fmt: tools
	goimports -w .
	golines -w .

# Export Buf registry dependencies to local files for IDEs/proto tooling that
# can't resolve imports from the Buf registry (e.g. buf/validate/validate.proto).
proto-deps: tools
	mkdir -p proto/_deps
	buf export buf.build/bufbuild/protovalidate -o proto/_deps
	buf export buf.build/googleapis/googleapis -o proto/_deps

endif
