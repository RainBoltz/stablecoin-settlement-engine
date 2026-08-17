// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Test} from "forge-std/Test.sol";
import {IERC20} from "../../src/interfaces/IERC20.sol";

/// @dev 主網 TetherToken 我們用得到的那一小塊介面。注意：沒有回傳值。
interface ITetherToken {
    function transfer(address to, uint256 value) external;
    function approve(address spender, uint256 value) external;
    function allowance(address owner, address spender) external view returns (uint256);
    function balanceOf(address who) external view returns (uint256);
    function basisPointsRate() external view returns (uint256);
    function maximumFee() external view returns (uint256);
    function paused() external view returns (bool);
}

/// @dev 跟 Day 2 的 StandardIntegrator 一樣：一個以為自己在跟標準 ERC-20 打交道的整合方。
///      returndata 解碼失敗發生在呼叫端，要多包這一層 vm.expectRevert 才攔得到。
contract StandardIntegrator {
    function doTransfer(address token, address to, uint256 value) external returns (bool) {
        return IERC20(token).transfer(to, value);
    }
}

/// @title USDTMainnetForkTest
/// @notice Day 3：mock 到底忠不忠實？把 Day 2 的斷言拿去對主網上真正的 USDT 跑一遍。
/// @dev 沒設 ETH_RPC_URL 就整組 skip，`forge test` 預設保持離線。
///      跑法：ETH_RPC_URL=https://... forge test --match-path 'test/fork/*' -vv
///      合約：https://etherscan.io/address/0xdAC17F958D2ee523a2206206994597C13D831ec7#code
contract USDTMainnetForkTest is Test {
    address internal constant USDT = 0xdAC17F958D2ee523a2206206994597C13D831ec7;

    address internal alice;
    address internal bob;
    StandardIntegrator internal integrator;
    bool internal forked;

    function setUp() public {
        string memory url = vm.envOr("ETH_RPC_URL", string(""));
        if (bytes(url).length == 0) return;
        vm.createSelectFork(url);
        forked = true;

        alice = makeAddr("alice");
        bob = makeAddr("bob");
        integrator = new StandardIntegrator();
        // 後門：沒人會給我們真 USDT，直接把 balance slot 寫進去
        deal(USDT, alice, 1_000e6);
        deal(USDT, address(integrator), 1_000e6);
    }

    modifier onlyFork() {
        vm.skip(!forked);
        _;
    }

    function test_realUsdt_dealFindsTheBalanceSlot() public onlyFork {
        assertEq(ITetherToken(USDT).balanceOf(alice), 1_000e6);
    }

    /// @dev Day 2 整篇文章濃縮成一行斷言：呼叫成功、returndata 是 0 bytes。
    function test_realUsdt_lowLevelCallSucceedsAndReturnsNothing() public onlyFork {
        vm.prank(alice);
        (bool ok, bytes memory ret) = USDT.call(abi.encodeCall(IERC20.transfer, (bob, 100e6)));
        assertTrue(ok);
        assertEq(ret.length, 0);
        assertEq(ITetherToken(USDT).balanceOf(bob), 100e6);
    }

    function test_realUsdt_revertsThroughStandardInterface() public onlyFork {
        vm.expectRevert();
        integrator.doTransfer(USDT, bob, 100e6);
        assertEq(ITetherToken(USDT).balanceOf(bob), 0);
    }

    function test_realUsdt_approveZeroLock() public onlyFork {
        vm.startPrank(alice);
        ITetherToken(USDT).approve(bob, 100e6);
        vm.expectRevert();
        ITetherToken(USDT).approve(bob, 50e6);
        ITetherToken(USDT).approve(bob, 0);
        ITetherToken(USDT).approve(bob, 50e6);
        vm.stopPrank();
        assertEq(ITetherToken(USDT).allowance(alice, bob), 50e6);
    }

    /// @dev 只斷言上限（setParams 規定 bps < 20、maxFee < 50 USDT），不斷言現在的值，因為 Tether 隨時可以改。
    function test_realUsdt_taxLeversExistAndAreWithinBounds() public onlyFork {
        assertLt(ITetherToken(USDT).basisPointsRate(), 20);
        assertLt(ITetherToken(USDT).maximumFee(), 50e6);
        assertFalse(ITetherToken(USDT).paused());
    }
}
