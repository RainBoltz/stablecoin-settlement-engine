// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Test} from "forge-std/Test.sol";
import {TokenZooBase} from "../script/TokenZooBase.sol";
import {DevnetAccounts} from "../script/DevnetAccounts.sol";
import {ERC20Mock} from "../src/mocks/ERC20Mock.sol";
import {USDTMock} from "../src/mocks/USDTMock.sol";
import {NoRevertERC20Mock} from "../src/mocks/NoRevertERC20Mock.sol";
import {FeeOnTransferERC20Mock} from "../src/mocks/FeeOnTransferERC20Mock.sol";

/// @title DevnetTest
/// @notice Day 3：devnet 的部署與注入邏輯是純 Solidity，所以測試直接繼承它，對做出來的世界狀態下斷言。
/// @dev 這組測試過了，`make devnet` 在 Anvil 上做出來的就是同一個世界，差別只在誰簽名。
///      也就是說部署腳本本身在測試覆蓋範圍內，不是一段「應該沒問題」的膠水。
contract DevnetTest is Test, TokenZooBase {
    Zoo internal zoo;
    DevnetAccounts.Cast internal who;

    function setUp() public {
        who = DevnetAccounts.cast();
        zoo = deploy(address(this)); // 測試合約扮演 deployer / 發行商
        seed(zoo, who);
    }

    // ====================================================================
    // 角色表：同一組助記詞，到哪裡都是同一批人
    // ====================================================================

    /// @dev Anvil 預設助記詞的 index 0/1/2/3/9。這些地址是公開的，永遠不要在真鏈上放錢。
    function test_castMatchesAnvilDefaultAccounts() public view {
        assertEq(who.deployer, 0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266);
        assertEq(who.payer, 0x70997970C51812dc3A010C7d01b50e0d17dc79C8);
        assertEq(who.merchant, 0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC);
        assertEq(who.relayer, 0x90F79bf6EB2c4f870365E785982E1f101E93b906);
        assertEq(who.blacklisted, 0xa0Ee7A142d267C1f36714E4a8F75612F20a79720);
    }

    // ====================================================================
    // 世界狀態
    // ====================================================================

    function test_payerAndMerchantAreFunded() public view {
        assertEq(ERC20Mock(zoo.usdc).balanceOf(who.payer), STABLE_SEED);
        assertEq(ERC20Mock(zoo.usdc).balanceOf(who.merchant), STABLE_SEED);
        assertEq(USDTMock(zoo.usdt).balanceOf(who.payer), STABLE_SEED);
        assertEq(USDTMock(zoo.usdt).balanceOf(who.merchant), STABLE_SEED);
        assertEq(NoRevertERC20Mock(zoo.noRevert).balanceOf(who.payer), TOKEN_SEED);
        assertEq(FeeOnTransferERC20Mock(zoo.feeOnTransfer).balanceOf(who.payer), TOKEN_SEED);
    }

    function test_issuerOwnsUsdtAndKeepsTreasury() public view {
        USDTMock usdt = USDTMock(zoo.usdt);
        assertEq(usdt.owner(), address(this));
        assertEq(usdt.balanceOf(address(this)), ISSUER_RESERVE);
        assertEq(usdt.totalSupply(), ISSUER_RESERVE + 3 * STABLE_SEED);
        assertEq(usdt.basisPointsRate(), 0, "tax stays dormant by default");
    }

    /// @dev 「永遠失敗」類的長相：手上有錢，但一毛都動不了。
    function test_blacklistedHoldsFundsItCannotMove() public {
        USDTMock usdt = USDTMock(zoo.usdt);
        assertEq(usdt.balanceOf(who.blacklisted), STABLE_SEED);
        vm.prank(who.blacklisted);
        vm.expectRevert(bytes("USDTMock: sender is blacklisted"));
        usdt.transfer(who.merchant, 1e6);
    }

    function test_relayerHasNoTokens() public view {
        assertEq(ERC20Mock(zoo.usdc).balanceOf(who.relayer), 0);
        assertEq(USDTMock(zoo.usdt).balanceOf(who.relayer), 0);
    }

    // ====================================================================
    // 後門：forge-std 的 deal 用探測 storage 的方式直接改餘額
    // ====================================================================

    /// @dev deal 的原理：vm.record() 錄下 balanceOf(who) 讀了哪些 slot，再逐一改值看回傳值會不會變。
    ///      四隻 mock 的 balance 都存在普通 mapping 裡，所以都找得到。
    function test_dealWorksOnEveryZooMember() public {
        address someone = makeAddr("someone");
        deal(zoo.usdc, someone, 7e6);
        deal(zoo.usdt, someone, 7e6);
        deal(zoo.noRevert, someone, 7e18);
        deal(zoo.feeOnTransfer, someone, 7e18);
        assertEq(ERC20Mock(zoo.usdc).balanceOf(someone), 7e6);
        assertEq(USDTMock(zoo.usdt).balanceOf(someone), 7e6);
        assertEq(NoRevertERC20Mock(zoo.noRevert).balanceOf(someone), 7e18);
        assertEq(FeeOnTransferERC20Mock(zoo.feeOnTransfer).balanceOf(someone), 7e18);
    }

    /// @dev 後門的代價：deal 只改 balanceOf，totalSupply 不動、Transfer 事件不發。
    ///      餘額加總從此不等於總供給，靠事件對帳的 listener 也看不到這筆錢。
    ///      所以種子狀態一律走正門，後門留給「測試裡臨時要一筆錢」與「fork 模式沒有正門鑰匙」。
    function test_dealDoesNotTouchTotalSupplyUnlessAsked() public {
        USDTMock usdt = USDTMock(zoo.usdt);
        uint256 supplyBefore = usdt.totalSupply();
        deal(zoo.usdt, who.relayer, 5e6);
        assertEq(usdt.totalSupply(), supplyBefore, "balances no longer sum to totalSupply");
        deal(zoo.usdt, who.relayer, 9e6, true); // 5e6 -> 9e6，adjust=true 只補上差額
        assertEq(usdt.totalSupply(), supplyBefore + 4e6);
    }

    // ====================================================================
    // 交接檔：deployments/<chainId>.json 寫得出去、讀得回來
    // ====================================================================

    function test_deploymentsJsonRoundTrips() public {
        string memory json = toJson(zoo, address(this));
        assertEq(vm.parseJsonUint(json, ".chainId"), block.chainid);
        assertEq(vm.parseJsonAddress(json, ".deployer"), address(this));
        assertEq(vm.parseJsonAddress(json, ".tokens.USDC.address"), zoo.usdc);
        assertEq(vm.parseJsonAddress(json, ".tokens.USDT.address"), zoo.usdt);
        assertEq(vm.parseJsonUint(json, ".tokens.USDT.decimals"), 6);
        assertEq(vm.parseJsonString(json, ".tokens.USDT.kind"), "USDTMock");
        assertEq(vm.parseJsonAddress(json, ".tokens.NRUSD.address"), zoo.noRevert);
        assertEq(vm.parseJsonAddress(json, ".tokens.FOTUSD.address"), zoo.feeOnTransfer);
        assertEq(vm.parseJsonAddress(json, ".accounts.payer"), who.payer);
        assertEq(vm.parseJsonAddress(json, ".accounts.blacklisted"), who.blacklisted);
    }
}
