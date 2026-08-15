# stablecoin-settlement-engine
Move money exactly once. A multichain stablecoin settlement engine.

## Repository layout

The repository is a monorepo. Only the EVM contracts exist so far; the relayer and the
non-EVM chains land in their own directories as the series progresses.

```
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
    └── test/
        └── TokenZoo.t.sol        # One test per trap, plus a conservation fuzz test
```

The mocks live under `src/` rather than `test/` on purpose: the devnet deployment scripts
and the relayer integration tests reuse them.

## Running the EVM tests

Install [Foundry](https://getfoundry.sh) if you do not have it:

```bash
curl -L https://foundry.paradigm.xyz | bash && foundryup
```

`forge-std` is a git submodule, so fetch it after cloning:

```bash
git submodule update --init --recursive
```

Then run the test suite:

```bash
cd contracts/evm
forge test
```

Useful variants: `forge test -vvvv` to see the full call traces (handy for the USDT
return-data failure), and `forge fmt` before committing.
