.PHONY: help test evm-build evm-test evm-test-fork ton-test go-test api-run devnet devnet-down devnet-reset devnet-seed devnet-status devnet-logs

help:
	@echo "make test            everything that runs offline: evm-test + go-test"
	@echo "make evm-build       compile contracts/evm"
	@echo "make evm-test        run the offline Solidity test suite (fork tests are skipped)"
	@echo "make evm-test-fork   run the mainnet-fork tests; needs ETH_RPC_URL"
	@echo "make ton-test        the Go-built W5 request against the real W5 wallet and real jetton contracts in @ton/sandbox; needs node"
	@echo "make go-test         vet + test the Go backend (backend/)"
	@echo "make api-run         start the Payment API on 127.0.0.1:8080 (memory stores, no chain)"
	@echo "make devnet          start anvil + deploy + seed (idempotent, state persists in .devnet/)"
	@echo "make devnet-down     stop anvil, dump state"
	@echo "make devnet-reset    stop, wipe state and deployments"
	@echo "make devnet-seed     redeploy + reseed against the running anvil"
	@echo "make devnet-status   who has what"
	@echo "make devnet-logs     tail anvil log"

test: evm-test go-test

evm-build:
	cd contracts/evm && forge build

evm-test:
	cd contracts/evm && forge test

evm-test-fork:
	@test -n "$$ETH_RPC_URL" || (echo "ETH_RPC_URL is not set" && exit 1)
	cd contracts/evm && forge test --match-path 'test/fork/*' -vv

ton-test:
	cd contracts/ton && (test -d node_modules || npm ci --no-audit --no-fund) && npm test
	cd backend && go test -count=1 -run 'TestTONSandbox' ./internal/chain/

go-test:
	cd backend && go vet ./... && go test ./...

api-run:
	cd backend && go run ./cmd/api

devnet:
	@scripts/devnet.sh up

devnet-down:
	@scripts/devnet.sh down

devnet-reset:
	@scripts/devnet.sh reset

devnet-seed:
	@scripts/devnet.sh seed

devnet-status:
	@scripts/devnet.sh status

devnet-logs:
	@scripts/devnet.sh logs
