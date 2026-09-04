# Vendored FunC sources

Unmodified copies, fetched 2026-09-04, kept here so `make ton-test` compiles the very code that runs on
mainnet instead of trusting a pre-built cell. They are test fixtures for this repository, nothing here is deployed.

| Directory | Upstream | Commit | Files |
| --- | --- | --- | --- |
| `ft/` | https://github.com/ton-blockchain/token-contract (`ft/`, the TEP-74 reference jetton; `stdlib.fc` from the repository root; MIT-style licence, TON Foundation 2023) | `1182ad99413242f09925d50e70ccb7e0e09f94d4` (2025-06-09) | jetton-minter.fc, jetton-wallet.fc, jetton-utils.fc, op-codes.fc, params.fc, stdlib.fc |
| `usdt/` | https://github.com/ton-blockchain/stablecoin-contract (`contracts/`, the jetton behind USDT on TON; MIT) | `5a3b500267b0bdfc6505a08e5ac661c805cab8b0` (2025-08-21) | jetton-minter.fc, jetton-wallet.fc, jetton-utils.fc, op-codes.fc, gas.fc, workchain.fc, stdlib.fc |

`ft/` has no `#include` lines: its build concatenates the files, and `compile.mjs` passes them in the
same order. `usdt/` resolves its own `#include`s. `compile.mjs` prints each code hash so it can be
compared with what an explorer shows for a live jetton wallet.
