# stablecoin-settlement-engine
Move money exactly once. A multichain stablecoin settlement engine.

## Repository layout

The repository is a monorepo: Solidity under `contracts/`, the Go off-chain side under `backend/`.
Only the EVM contracts, the local devnet tooling, the Payment Intent state machine, the PaymentRef
derivation, the first slice of the Payment API (create / get / trace an intent, behind an
idempotency layer), the double-entry ledger, the job queue and the first slice of the relayer (a
single worker that takes an authorized intent to `confirming` through a fake sender) exist so far;
the real chain senders and the non-EVM chains land in their own directories as the series progresses.

```
Makefile                          # Entry points: make test, make devnet, make evm-test, make go-test, ...
scripts/
└── devnet.sh                     # Local devnet lifecycle: up / down / reset / seed / status
backend/                          # Go module (stdlib only so far), go 1.24
├── cmd/
│   └── api/                      # `make api-run`: the Payment API on memory stores, for curl
└── internal/
    ├── paymentref/               # PaymentRef: sha256 commitment over (intent id + payment terms), 32 bytes, goes on-chain
    │   ├── ref.go                # Terms, Derive, Preimage (length-prefixed), Ref.String / Parse (0x + 64 hex)
    │   └── *_test.go             # pinned vector, every-field-matters, boundary, parse; Example_derive
    ├── intent/                   # Payment Intent state machine: states, actors, transition table, CAS store
    │   ├── state.go              # State / Actor / Rule and the transition table (Rules())
    │   ├── intent.go             # Intent (ID + Ref), Transition (history entry), New() derives the Ref, Terms()
    │   ├── machine.go            # Apply(): replay is a no-op, terminal is absorbing, actor + evidence checks
    │   ├── store.go              # Store interface (CAS Save that re-derives the Ref, Get, GetByRef), MemoryStore, Advance()
    │   ├── table.go              # Table(): the transition table as text, pinned by the golden test
    │   ├── testdata/transitions.golden
    │   └── *_test.go             # Rules_*, Apply_*, MemoryStore_*, ref_test.go, Example_lifecycle
    ├── ledger/                   # Double-entry ledger: append-only, hash-chained journal of hold / post / void entries, keyed by PaymentRef
    │   ├── ledger.go             # Asset, Account (payer: / merchant: / fee:), Leg, Kind, Entry, Validate (legs sum to zero)
    │   ├── hash.go               # Preimage + sha256 chain: every entry hashes the previous one
    │   ├── journal.go            # Journal interface (Append only, idempotent by id), MemoryJournal, Balances projection, Verify
    │   └── *_test.go             # Entry_*, Journal_* (idempotent append, resolve once, pending / posted, pinned chain head, tampering), Example_holdPostVoid
    ├── queue/                    # Job queue: at-least-once, lease (visibility timeout), receipt, ack / nack; jobs carry only intent id + ref
    │   ├── queue.go              # Job (id = <intent id>/settle), Delivery, Receipt, Queue interface, errors
    │   ├── memory.go             # MemoryQueue: FIFO among visible jobs, idempotent enqueue while pending, attempt counter
    │   └── *_test.go             # Job_Validate, MemoryQueue_* (idempotent enqueue, lease hides, stale receipt, nack delay, concurrency), Example_leaseAckNack
    ├── relayer/                  # Relayer worker: lease a job, read the intent, settling -> hold -> send -> confirming, ack last
    │   ├── relayer.go            # Sender interface, Worker (RunOnce / Run), Outcome (sent / no-op / retry / needs_review), Config, process()
    │   └── *_test.go             # Worker_* (order pinned, redelivery no-op, lease takeover, send failure -> retry -> review, lost CAS, many workers), Example_settleThroughQueue
    ├── idempotency/              # Idempotency-Key layer: scope + key + fingerprint -> one execution, one answer
    │   ├── key.go                # Scope, Key (validation), Fingerprint (sha256 of method/path/raw body)
    │   ├── store.go              # Record, Store (atomic Claim, Complete with attempt CAS), MemoryStore
    │   ├── handler.go            # http middleware: 400 / 401 / 409 / 422, replay with Idempotent-Replayed
    │   └── *_test.go             # Key_*, Fingerprint_*, MemoryStore_*, Handler_*
    └── api/                      # Payment API: POST /v1/payment_intents (idempotent), GET /v1/payment_intents/{id},
        │                         #              GET /v1/payment_refs/{ref} (intent + history, for whoever only holds the ref)
        ├── api.go                # routes, request/response shapes, NewIntentID, TraceResponse
        └── *_test.go             # CreateIntent_*, GetIntent_*, TraceRef_*, Example_retryStorm, Example_traceByRef
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
make api-run         # Payment API on http://127.0.0.1:8080, memory stores, no chain
make devnet          # start anvil, deploy the Token Zoo, seed balances; state persists in .devnet/
make devnet-status   # who has what
make devnet-down     # stop anvil (state is dumped to .devnet/anvil-state.json)
make devnet-reset    # wipe state and deployments; the next `make devnet` starts from genesis
```

Deployed addresses land in `contracts/evm/deployments/31337.json`. To run the mainnet-fork
faithfulness tests: `ETH_RPC_URL=https://... make evm-test-fork`. Useful variants:
`forge test -vvvv` inside `contracts/evm` to see full call traces, and `forge fmt` before committing.

The Payment API requires `Authorization: Bearer <token>` (the token is the idempotency scope; there is
no real authentication yet) and an `Idempotency-Key` header on every `POST`. Same key + same body replays
the first answer with `Idempotent-Replayed: true`; same key + different body is a `422`; a retry that
lands while the first request is still running is a `409`. Keys live 24 hours.
`Example_retryStorm` and `Example_traceByRef` (`internal/api/example_test.go`) walk through the retry storm
and a trace-by-ref end to end; `go test ./internal/api -run Example -v` runs them.

Every intent carries a `ref`: `sha256` over a domain tag, the intent id and the payment terms
(chain, token, payer, merchant, amount), 32 bytes, printed as `0x` + 64 hex. It is the only key that
goes on-chain; the intent store re-derives it on every save and `GET /v1/payment_refs/{ref}` walks back
from a ref to the intent and its history. `Example_derive` (`internal/paymentref/example_test.go`) shows one.

The ledger (`internal/ledger`) is double-entry and append-only. Every entry has at least two legs in
one asset that sum to zero; `hold` reserves the amount before the relayer broadcasts, `post` settles
with what actually arrived on-chain (a third `fee:` leg absorbs transfer tax), `void` releases a hold
that will never move money. A hold resolves exactly once, `Append` is idempotent by entry id, pending /
posted balances are a projection over the journal, and every entry hashes the previous one so `Verify`
detects any edit. `Example_holdPostVoid` (`internal/ledger/example_test.go`) walks three payments through it.

The relayer side starts with two packages. `internal/queue` is an at-least-once job queue in the shape
of SQS: `Enqueue` is idempotent by job id while the job is pending, `Lease` hides a job for a lease
period and hands out a receipt, `Ack` / `Nack` require the current receipt (a worker that lost its lease
gets `ErrStaleReceipt`), and a job carries only the intent id and ref, never the payload. `internal/relayer`
is one worker loop over that queue: read the intent, CAS it to `settling`, append the ledger `hold`, call
the `Sender`, CAS to `confirming`, ack last. Every step is idempotent, so redelivery is harmless; a
redelivered job that finds the intent already in `settling` without a tx hash does not resend (it cannot
tell whether the previous broadcast left the building): it waits while the intent is young and pushes it
to `needs_review` after `StuckAfter`. There is no real chain sender yet; `Example_settleThroughQueue`
(`internal/relayer/example_test.go`) walks a normal job, a lease takeover and a send failure through it.

The Payment Intent transition table is pinned by `backend/internal/intent/testdata/transitions.golden`;
to change a rule, edit `Rules()` and regenerate with `cd backend && go test ./internal/intent -run Golden -update`,
so the diff shows up in review. `go test ./internal/intent -run Example -v` walks one intent through its lifecycle.

Everything under `make devnet` uses Anvil's default mnemonic. Those keys are public; never point
the scripts at a real network.
