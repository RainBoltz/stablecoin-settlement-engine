// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Test} from "forge-std/Test.sol";

import {SafeTransferCaller} from "../SafeTransfer.t.sol";

/// @dev 主網 TetherToken 這裡用得到的那一小塊介面。注意：approve 沒有回傳值。
interface ITetherToken {
    function approve(address spender, uint256 value) external;
    function balanceOf(address who) external view returns (uint256);
}

/// @title SafeTransferMainnetForkTest
/// @notice mock 上過了不算數：把 SafeTransfer 拿去對主網上真正的 USDT 跑一遍。
/// @dev 沒設 ETH_RPC_URL 就整組 skip，`forge test` 預設保持離線。
///      跑法：ETH_RPC_URL=https://... forge test --match-path 'test/fork/*' -vv
///      合約：https://etherscan.io/address/0xdAC17F958D2ee523a2206206994597C13D831ec7#code
contract SafeTransferMainnetForkTest is Test {
    address internal constant USDT = 0xdAC17F958D2ee523a2206206994597C13D831ec7;

    address internal alice;
    address internal bob;
    SafeTransferCaller internal caller;
    bool internal forked;

    function setUp() public {
        string memory url = vm.envOr("ETH_RPC_URL", string(""));
        if (bytes(url).length == 0) return;
        vm.createSelectFork(url);
        forked = true;

        alice = makeAddr("alice");
        bob = makeAddr("bob");
        caller = new SafeTransferCaller();
        // secondary gate：沒人會給我們真 USDT，直接把 balance slot 寫進去
        deal(USDT, alice, 1_000e6);
        deal(USDT, address(caller), 1_000e6);
    }

    modifier onlyFork() {
        vm.skip(!forked);
        _;
    }

    /// @dev 標準介面過不了的那筆轉帳，封裝之後對真 USDT 走得完。
    function test_realUsdt_safeTransferMovesRealTether() public onlyFork {
        caller.doTransfer(USDT, bob, 100e6);
        assertEq(ITetherToken(USDT).balanceOf(bob), 100e6);
    }

    /// @dev transferFrom 版：結算合約每天在走的那條路，對真 USDT 也走得完。
    function test_realUsdt_safeTransferFromMovesRealTether() public onlyFork {
        vm.prank(alice);
        ITetherToken(USDT).approve(address(caller), 100e6);

        caller.doTransferFrom(USDT, alice, bob, 100e6);
        assertEq(ITetherToken(USDT).balanceOf(alice), 900e6);
        assertEq(ITetherToken(USDT).balanceOf(bob), 100e6);
    }
}
