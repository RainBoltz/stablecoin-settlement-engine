# stablecoin-settlement-engine
Move money exactly once. A multichain stablecoin settlement engine.

## Repository layout

The repository is a monorepo: Solidity under `contracts/`, the Go off-chain side under `backend/`.
Only the EVM contracts, the local devnet tooling and the Payment Intent state machine exist so far;
the relayer and the non-EVM chains land in their own directories as the series progresses.

```
Makefile                          # Entry points: make test, make devnet, make evm-test, make go-test, ...
scripts/
└── devnet.sh                     # Local devnet lifecycle: up / down / reset / seed / status
backend/                          # Go module (stdlib only so far), go 1.24
└── internal/
    └── intent/                   # Payment Intent state machine: states, actors, transition table, CAS store
        ├── state.go              # State / Actor / Rule and the transition table (Rules())
        ├── intent.go             # Intent, Transition (history entry), New()
        ├── machine.go            # Apply(): replay is a no-op, terminal is absorbing, actor + evidence checks
        ├── store.go              # Store interface (compare-and-swap Save), MemoryStore, Advance()
        ├── table.go              # Table(): the transition table as text, pinned by the golden test
        ├── testdata/transitions.golden
        └── *_test.go             # Rules_*, Apply_*, MemoryStore_*, Example_lifecycle
contracts/
└── evm/                          # Foundry project, Solidity 0.8.26, forge-std only
    ├── foundry.toml
    ├── src/
    │   ├── interfaces/
    │   │   └── IERC20.sol        # Minimal EIP-20 interface, written from the spec
    │   └── mocks/                # Mock token zoo: real-world ERC-20 misbehaviour
    │       ├── ERC20Mock.sol             # Fully compliant baseline
    │       ├── USDTMock.sol              # No return value, approve race lock, fee, blacklist, pause
    │       ├── NoRevertERC20Mock.sol     # Returns false instead of reverting
    │       └── FeeOnTransferERC20Mock.sol# Recipient always receives less than was sent
    ├── script/
    │   ├── DevnetAccounts.sol    # The cast: deployer / payer / merchant / relayer / blacklisted
    │   ├── TokenZooBase.sol      # deploy() + seed() + deployments json, no run(); tests inherit it
    │   ├── DeployTokenZoo.s.sol  # run(): broadcast the deployment, write deployments/<chainId>.json
    │   └── SeedDevnet.s.sol      # run(): read the json, broadcast the world state
    ├── deployments/              # Generated, git-ignored. Off-chain code reads addresses from here
    └── test/
        ├── TokenZoo.t.sol        # One test per trap, plus a conservation fuzz test
        ├── Devnet.t.sol          # Inherits TokenZooBase and asserts the seeded world state
        └── fork/
            └── USDTMainnet.t.sol # Same assertions against the real USDT on a mainnet fork (needs ETH_RPC_URL)
```

The mocks live under `src/` rather than `test/` on purpose: the devnet deployment scripts
and the relayer integration tests reuse them.

## Quick start

Requires [Foundry](https://getfoundry.sh) >= 1.3.0 (`anvil_dealERC20` was added there), `jq`,
and [Go](https://go.dev/dl/) >= 1.24 for `backend/`.

```bash
curl -L https://foundry.paradigm.xyz | bash && foundryup
git submodule update --init --recursive     # forge-std is a git submodule
```

```bash
make test            # everything that runs offline: evm-test + go-test
make evm-test        # Solidity suite; the mainnet-fork tests are skipped
make go-test         # go vet + go test for backend/
make devnet          # start anvil, deploy the Token Zoo, seed balances; state persists in .devnet/
make devnet-status   # who has what
make devnet-down     # stop anvil (state is dumped to .devnet/anvil-state.json)
make devnet-reset    # wipe state and deployments; the next `make devnet` starts from genesis
```

Deployed addresses land in `contracts/evm/deployments/31337.json`. To run the mainnet-fork
faithfulness tests: `ETH_RPC_URL=https://... make evm-test-fork`. Useful variants:
`forge test -vvvv` inside `contracts/evm` to see full call traces, and `forge fmt` before committing.

The Payment Intent transition table is pinned by `backend/internal/intent/testdata/transitions.golden`;
to change a rule, edit `Rules()` and regenerate with `cd backend && go test ./internal/intent -run Golden -update`,
so the diff shows up in review. `go test ./internal/intent -run Example -v` walks one intent through its lifecycle.

Everything under `make devnet` uses Anvil's default mnemonic. Those keys are public; never point
the scripts at a real network.
