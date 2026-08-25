// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Settlement} from "../../src/Settlement.sol";
import {IERC20} from "../../src/interfaces/IERC20.sol";
import {ISignatureTransfer} from "../../src/interfaces/ISignatureTransfer.sol";
import {Permit2Signing} from "../SettlementPermit2.t.sol";

/// @dev 主網 TetherToken 這裡用得到的那一小塊介面。注意：approve 沒有回傳值。
interface ITetherToken {
    function approve(address spender, uint256 value) external;
    function balanceOf(address who) external view returns (uint256);
}

/// @title SettlementPermit2MainnetForkTest
/// @notice 替身上過了不算數：同一份簽名拿去對主網上真正的 Permit2 重放一次。
/// @dev 這組測試釘的是「我們算 digest 的方式跟 Uniswap 部署的那份合約一致」——
///      witness 型別字串、欄位順序、domain 少一個字都會變成 InvalidSigner。
///      沒設 ETH_RPC_URL 就整組 skip，`forge test` 預設保持離線。
///      跑法：ETH_RPC_URL=https://... forge test --match-path 'test/fork/*' -vv
///      合約：https://etherscan.io/address/0x000000000022D473030F116dDEE9F6B43aC78BA3#code
contract SettlementPermit2MainnetForkTest is Permit2Signing {
    address internal constant PERMIT2 = 0x000000000022D473030F116dDEE9F6B43aC78BA3;
    address internal constant USDC = 0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48;
    address internal constant USDT = 0xdAC17F958D2ee523a2206206994597C13D831ec7;

    address internal owner;
    address internal relayer;
    address internal payer;
    uint256 internal payerKey;
    address internal merchant;

    Settlement internal settlement;
    uint256 internal deadline;
    bool internal forked;

    function setUp() public {
        string memory url = vm.envOr("ETH_RPC_URL", string(""));
        if (bytes(url).length == 0) return;
        vm.createSelectFork(url);
        forked = true;

        owner = makeAddr("owner");
        relayer = makeAddr("relayer");
        merchant = makeAddr("merchant");
        (payer, payerKey) = makeAddrAndKey("payer");

        vm.prank(owner);
        settlement = new Settlement(PERMIT2);
        vm.prank(owner);
        settlement.setRelayer(relayer, true);

        // secondary gate：沒人會給我們真的 USDC 或 USDT，直接把 balance slot 寫進去
        deal(USDC, payer, 1_000e6);
        deal(USDT, payer, 1_000e6);

        deadline = block.timestamp + 15 minutes;
    }

    modifier onlyFork() {
        vm.skip(!forked);
        _;
    }

    /// @dev domain separator 從鏈上那份合約讀，不自己算，這樣簽出來的東西才是真的能用的。
    function signFor(address token, bytes32 ref) internal view returns (bytes memory) {
        return signPayment(
            payerKey,
            ISignatureTransfer(PERMIT2).DOMAIN_SEPARATOR(),
            address(settlement),
            token,
            100e6,
            ref,
            merchant,
            deadline
        );
    }

    /// @dev 快樂路徑對真 Permit2：payer 一筆交易都沒發，只給了 Permit2 一次性授權加一份簽名。
    function test_realPermit2_settlesUsdcWithASignature() public onlyFork {
        vm.prank(payer);
        IERC20(USDC).approve(PERMIT2, type(uint256).max);

        bytes32 ref = keccak256("day-18/fork/usdc");
        vm.prank(relayer);
        settlement.settleWithPermit(USDC, payer, merchant, 100e6, ref, deadline, signFor(USDC, ref));

        assertEq(IERC20(USDC).balanceOf(merchant), 100e6);
        assertEq(IERC20(USDC).balanceOf(payer), 900e6);
    }

    /// @dev 不回傳值的那顆也走得完：搬錢那一步在 Permit2 裡面，它自己也做了安全封裝。
    function test_realPermit2_settlesRealTetherWithASignature() public onlyFork {
        vm.prank(payer);
        ITetherToken(USDT).approve(PERMIT2, type(uint256).max);

        bytes32 ref = keccak256("day-18/fork/usdt");
        vm.prank(relayer);
        settlement.settleWithPermit(USDT, payer, merchant, 100e6, ref, deadline, signFor(USDT, ref));

        assertEq(ITetherToken(USDT).balanceOf(merchant), 100e6);
        assertEq(ITetherToken(USDT).balanceOf(payer), 900e6);
    }
}
