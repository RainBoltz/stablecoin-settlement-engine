// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Test} from "forge-std/Test.sol";

import {IERC20} from "../src/interfaces/IERC20.sol";
import {SafeTransfer} from "../src/libraries/SafeTransfer.sol";
import {ERC20Mock} from "../src/mocks/ERC20Mock.sol";
import {NoRevertERC20Mock} from "../src/mocks/NoRevertERC20Mock.sol";
import {USDTMock} from "../src/mocks/USDTMock.sol";

/// @dev 跟 Day 2 的 StandardIntegrator 同一個角色：一個真的用 SafeTransfer 搬錢的整合方。
///      library 的 internal 函式會被編譯進呼叫端，vm.expectRevert 攔的是外部呼叫，
///      所以每一條測試都繞經這個 harness，revert 才會發生在一個攔得到的地方。
contract SafeTransferCaller {
    using SafeTransfer for IERC20;

    function doTransfer(address token, address to, uint256 value) external {
        IERC20(token).safeTransfer(to, value);
    }

    function doTransferFrom(address token, address from, address to, uint256 value) external {
        IERC20(token).safeTransferFrom(from, to, value);
    }
}

/// @title SafeTransferTest
/// @notice 把 SafeTransfer 的三條判讀路徑全部釘死：revert 轉發、0 bytes 當成功（但要有 code）、
///         false 轉成明確的 revert。
/// @dev 對照組（完全合規的 token）也要測：封裝的第一守則是不能弄壞本來就好的東西。
contract SafeTransferTest is Test {
    address internal owner;
    address internal payer;
    address internal merchant;

    SafeTransferCaller internal caller;
    ERC20Mock internal usdc;
    USDTMock internal usdt;
    NoRevertERC20Mock internal bad;

    /// @dev 6 位小數：100e6 就是 100 USDC。
    uint256 internal constant AMOUNT = 100e6;
    uint256 internal constant SEED = 1_000_000e6;

    function setUp() public {
        owner = makeAddr("owner");
        payer = makeAddr("payer");
        merchant = makeAddr("merchant");

        caller = new SafeTransferCaller();
        usdc = new ERC20Mock("USD Coin (mock)", "USDC", 6);
        vm.prank(owner);
        usdt = new USDTMock();
        bad = new NoRevertERC20Mock("No Revert USD", "NRUSD", 18);
    }

    // ====================================================================
    // 對照組：完全合規的 token，封裝不能弄壞它
    // ====================================================================

    /// @dev 合規 token 回傳 true，封裝解碼後放行，錢照常移動。
    function test_safeTransfer_compliantTokenStillWorks() public {
        usdc.mint(address(caller), SEED);
        caller.doTransfer(address(usdc), merchant, AMOUNT);
        assertEq(usdc.balanceOf(merchant), AMOUNT);
    }

    /// @dev transferFrom 的合規路徑：allowance 給 harness，錢從 payer 直達 merchant。
    function test_safeTransferFrom_compliantTokenStillWorks() public {
        usdc.mint(payer, SEED);
        vm.prank(payer);
        usdc.approve(address(caller), AMOUNT);

        caller.doTransferFrom(address(usdc), payer, merchant, AMOUNT);
        assertEq(usdc.balanceOf(payer), SEED - AMOUNT);
        assertEq(usdc.balanceOf(merchant), AMOUNT);
    }

    // ====================================================================
    // 0 bytes 的 returndata：USDT 這一型
    // ====================================================================

    /// @dev USDT 型的 token 成功時什麼都不回。標準介面死在解碼，封裝把 0 bytes 當成功收下。
    function test_safeTransfer_usdtStyleEmptyReturnIsSuccess() public {
        usdt.mint(address(caller), SEED);
        caller.doTransfer(address(usdt), merchant, AMOUNT);
        assertEq(usdt.balanceOf(merchant), AMOUNT);
    }

    /// @dev 同一件事的 transferFrom 版：這條就是結算合約每天在走的路。
    function test_safeTransferFrom_usdtStyleEmptyReturnIsSuccess() public {
        usdt.mint(payer, SEED);
        vm.prank(payer);
        usdt.approve(address(caller), AMOUNT);

        caller.doTransferFrom(address(usdt), payer, merchant, AMOUNT);
        assertEq(usdt.balanceOf(payer), SEED - AMOUNT);
        assertEq(usdt.balanceOf(merchant), AMOUNT);
    }

    /// @dev 0 bytes 也是「對沒有 code 的地址 call」的樣子，所以這條路要補查 code。
    ///      少了這個檢查，打錯 token 地址會變成一筆「成功」的空轉帳。
    function test_safeTransfer_revertsWhenTokenHasNoCode() public {
        vm.expectRevert(bytes("SafeTransfer: token has no code"));
        caller.doTransfer(makeAddr("not-a-token"), merchant, AMOUNT);
    }

    /// @dev 同一個檢查對 transferFrom 一樣生效。
    function test_safeTransferFrom_revertsWhenTokenHasNoCode() public {
        vm.expectRevert(bytes("SafeTransfer: token has no code"));
        caller.doTransferFrom(makeAddr("not-a-token"), payer, merchant, AMOUNT);
    }

    // ====================================================================
    // 回傳 false：轉成明確的 revert
    // ====================================================================

    /// @dev 失敗只回傳 false 的 token，封裝把它變回一筆看得見的失敗；
    ///      不封裝的話這一筆就是 Day 2 的幽靈支付。
    function test_safeTransfer_falseReturnBecomesRevert() public {
        // 刻意不給 harness 餘額：這顆 token 會回傳 false 而不是 revert
        vm.expectRevert(bytes("SafeTransfer: transfer returned false"));
        caller.doTransfer(address(bad), merchant, AMOUNT);
    }

    /// @dev transferFrom 版：沒有 allowance，token 回傳 false，封裝轉成 revert。
    function test_safeTransferFrom_falseReturnBecomesRevert() public {
        bad.mint(payer, SEED);
        vm.expectRevert(bytes("SafeTransfer: transfer returned false"));
        caller.doTransferFrom(address(bad), payer, merchant, AMOUNT);
    }

    // ====================================================================
    // token 自己 revert：原因要原封不動轉發
    // ====================================================================

    /// @dev 封裝不蓋 token 的 revert 原因：allowance 不足時，呼叫端看到的必須是
    ///      USDTMock 自己的訊息，鏈下的失敗分類才有線索可讀。
    function test_safeTransferFrom_tokenRevertReasonBubblesUp() public {
        usdt.mint(payer, SEED);
        // 刻意不 approve
        vm.expectRevert(bytes("USDTMock: insufficient allowance"));
        caller.doTransferFrom(address(usdt), payer, merchant, AMOUNT);
    }
}
