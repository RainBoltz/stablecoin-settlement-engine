// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Test} from "forge-std/Test.sol";

import {IERC20} from "../src/interfaces/IERC20.sol";
import {ERC20Mock} from "../src/mocks/ERC20Mock.sol";
import {FeeOnTransferERC20Mock} from "../src/mocks/FeeOnTransferERC20Mock.sol";
import {NoRevertERC20Mock} from "../src/mocks/NoRevertERC20Mock.sol";
import {USDTMock} from "../src/mocks/USDTMock.sol";

/// @notice 一個「以為自己在跟標準 ERC-20 打交道」的整合方合約。
/// @dev 為什麼要多包這一層：returndata 解碼失敗是發生在「呼叫端」而不是 token 裡面。
///      把呼叫放進獨立合約，這個 revert 才會出現在一次外部呼叫的邊界上，讓 vm.expectRevert 攔得到，
///      同時也更貼近真實情況——出事的一定是我們自己寫的整合合約，不是 token。
contract StandardIntegrator {
    function doTransfer(address token, address to, uint256 value) external returns (bool) {
        return IERC20(token).transfer(to, value);
    }

    function doApprove(address token, address spender, uint256 value) external returns (bool) {
        return IERC20(token).approve(spender, value);
    }
}

/// @title TokenZooTest
/// @notice 把 Token Zoo 裡每一種非標準行為釘死的測試。
/// @dev 這些測試的用途不是證明 mock「寫對了」，而是把每個真實世界的陷阱變成可重現的失敗案例。
///      Day 3 起的 devnet 部署腳本與 relayer 整合測試會直接重用 src/mocks 底下這些合約。
contract TokenZooTest is Test {
    // owner 是 USDTMock 的部署者，同時也是轉帳稅、黑名單與 pause 的權限持有者
    address internal owner;
    address internal alice;
    address internal bob;

    ERC20Mock internal standard;
    USDTMock internal usdt;
    NoRevertERC20Mock internal noRevert;
    FeeOnTransferERC20Mock internal feeToken;
    StandardIntegrator internal integrator;

    /// @dev bob 預先持有的 USDT 餘額。USDT 是 6 位小數，所以 1e6 = 1 USDT。
    uint256 internal constant BOB_USDT = 1_000_000e6;

    function setUp() public {
        owner = makeAddr("owner");
        alice = makeAddr("alice");
        bob = makeAddr("bob");

        standard = new ERC20Mock("Standard USD", "SUSD", 18);

        // 由 owner 部署，之後 setParams / addBlackList / pause 都只有它能呼叫
        vm.prank(owner);
        usdt = new USDTMock();

        noRevert = new NoRevertERC20Mock("No Revert USD", "NRUSD", 18);
        // 1% 轉帳稅，手續費收給 owner
        feeToken = new FeeOnTransferERC20Mock("Fee On Transfer USD", "FOTUSD", 18, 100, owner);
        integrator = new StandardIntegrator();

        usdt.mint(bob, BOB_USDT);
    }

    // ====================================================================
    // 對照組：完全符合 EIP-20 的 token
    // ====================================================================

    /// @dev 基準線：標準 token 透過標準 IERC20 介面呼叫，回傳 true、餘額正確、事件正確。
    ///      後面每一個「壞掉」的測試，都是拿這條路徑當對照。
    function test_standardMock_transferThroughStandardInterfaceSucceeds() public {
        standard.mint(alice, 100e18);
        IERC20 token = IERC20(address(standard));

        vm.expectEmit(true, true, false, true, address(standard));
        emit IERC20.Transfer(alice, bob, 40e18);

        vm.prank(alice);
        bool success = token.transfer(bob, 40e18);

        assertTrue(success, "compliant token must return true");
        assertEq(token.balanceOf(alice), 60e18, "sender balance");
        assertEq(token.balanceOf(bob), 40e18, "recipient balance");
        assertEq(token.totalSupply(), 100e18, "supply unchanged");
    }

    /// @dev 標準的 transferFrom 會照實扣掉 allowance。
    function test_standardMock_transferFromSpendsAllowance() public {
        standard.mint(alice, 100e18);

        vm.prank(alice);
        standard.approve(bob, 60e18);

        vm.prank(bob);
        bool success = standard.transferFrom(alice, bob, 25e18);

        assertTrue(success, "transferFrom must return true");
        assertEq(standard.allowance(alice, bob), 35e18, "allowance must be decremented");
        assertEq(standard.balanceOf(bob), 25e18, "recipient balance");
    }

    /// @dev 標準行為：餘額不足時 revert，呼叫端不可能沒發現。
    function test_standardMock_revertsOnInsufficientBalance() public {
        standard.mint(alice, 1e18);

        vm.prank(alice);
        vm.expectRevert(bytes("ERC20Mock: insufficient balance"));
        standard.transfer(bob, 2e18);
    }

    // ====================================================================
    // 陷阱一：USDT 風格的「沒有回傳值」
    // ====================================================================

    /// @dev 用 USDTMock 自己的型別呼叫完全正常——編譯器知道它沒有回傳值，不會去解碼。
    ///      所以「只用這個 token 的官方 ABI」的程式碼永遠不會踩到雷，問題只出在共用介面上。
    function test_usdtStyle_transferSucceedsWithNativeType() public {
        vm.prank(bob);
        usdt.transfer(alice, 100e6);

        assertEq(usdt.balanceOf(alice), 100e6, "recipient balance");
        assertEq(usdt.balanceOf(bob), BOB_USDT - 100e6, "sender balance");
    }

    /// @dev 同一個合約，改用標準 IERC20 介面呼叫就會 revert。
    ///      原因不在 token 裡面：USDTMock 執行得好好的，只是回傳 0 bytes，
    ///      而 Solidity 在把 returndata 解碼成 bool 之前，會先檢查長度夠不夠 32 bytes。
    ///      要命的是 revert 發生在「呼叫端」，token 那邊的狀態變更也會被一起 rollback。
    function test_usdtStyle_revertsThroughStandardInterface() public {
        // 先用低階 call 看清楚根本原因：呼叫本身成功，但 returndata 是空的
        vm.prank(bob);
        (bool rawSuccess, bytes memory returnData) =
            address(usdt).call(abi.encodeWithSignature("transfer(address,uint256)", alice, 0));
        assertTrue(rawSuccess, "USDTMock itself executes fine");
        assertEq(returnData.length, 0, "USDTMock returns no data, standard ABI expects 32 bytes");

        // 讓整合方合約自己持有一點餘額，確保待會的 revert 不是因為餘額不足
        vm.prank(bob);
        usdt.transfer(address(integrator), 100e6);

        // transfer：以標準介面呼叫 -> 解碼 bool 失敗 -> revert
        vm.expectRevert();
        integrator.doTransfer(address(usdt), alice, 1e6);

        // approve：同樣的原因，同樣的下場
        vm.expectRevert();
        integrator.doApprove(address(usdt), alice, 1e6);

        // view 函式的 ABI 是相容的，讀取一切正常——這正是這個 bug 難以在測試網被發現的原因
        assertEq(IERC20(address(usdt)).balanceOf(address(integrator)), 100e6, "read path is compatible");
        assertEq(IERC20(address(usdt)).totalSupply(), BOB_USDT, "read path is compatible");

        // 兩次 revert 讓 token 端的狀態變更全部回滾，錢沒動、額度也沒設定
        assertEq(usdt.balanceOf(alice), 0, "no tokens moved");
        assertEq(usdt.allowance(address(integrator), alice), 0, "approve never took effect");
    }

    // ====================================================================
    // 陷阱二：approve 歸零鎖
    // ====================================================================

    /// @dev 現有額度非 0 時不能直接改成另一個非 0 值，必須先歸零。
    ///      任何「每次結算前重設 allowance」的流程，如果沒有先 approve(0)，第二次就會炸。
    function test_usdtStyle_approveRequiresResetToZero() public {
        vm.startPrank(bob);

        usdt.approve(alice, 100e6);
        assertEq(usdt.allowance(bob, alice), 100e6, "first approve");

        // 直接調降額度 -> revert
        vm.expectRevert(bytes("USDTMock: approve from non-zero allowance"));
        usdt.approve(alice, 50e6);

        // 正確作法：先歸零，再設定新額度（代價是多一筆交易、多一次上鏈延遲）
        usdt.approve(alice, 0);
        usdt.approve(alice, 50e6);

        vm.stopPrank();

        assertEq(usdt.allowance(bob, alice), 50e6, "approve after reset to zero");
    }

    // ====================================================================
    // 陷阱三：休眠的轉帳稅
    // ====================================================================

    /// @dev 主網上 basisPointsRate 一直是 0，所以預設是 1:1 入帳；
    ///      但 setParams 一直都在，owner 隨時能單方面打開稅率，收款方就會短少。
    function test_usdtStyle_dormantFeeIsActivatedBySetParams() public {
        // 稅率休眠中：轉多少就入帳多少
        vm.prank(bob);
        usdt.transfer(alice, 1000e6);
        assertEq(usdt.balanceOf(alice), 1000e6, "1:1 while the fee is dormant");
        assertEq(usdt.balanceOf(owner), 0, "owner takes nothing");

        // owner 打開 0.1% 稅率、單筆上限 20 USDT，不需要任何持有者同意
        vm.prank(owner);
        usdt.setParams(10, 20);
        assertEq(usdt.basisPointsRate(), 10, "basis points rate");
        assertEq(usdt.maximumFee(), 20e6, "maximum fee is scaled by decimals");

        // 同樣的呼叫，入帳金額少了 1 USDT，差額進了 owner 的口袋
        vm.prank(bob);
        usdt.transfer(alice, 1000e6);
        assertEq(usdt.balanceOf(alice), 1000e6 + 999e6, "recipient is short by the fee");
        assertEq(usdt.balanceOf(owner), 1e6, "fee goes to the owner");
    }

    /// @dev 稅有上限：金額夠大時 fee 會被 maximumFee 封頂，不是無限比例抽下去。
    function test_usdtStyle_feeIsCappedByMaximumFee() public {
        vm.prank(owner);
        usdt.setParams(10, 20);

        // 未封頂的話 0.1% 會是 100 USDT，但 maximumFee 只有 20 USDT
        vm.prank(bob);
        usdt.transfer(alice, 100_000e6);

        assertEq(usdt.balanceOf(owner), 20e6, "fee is capped at maximumFee");
        assertEq(usdt.balanceOf(alice), 100_000e6 - 20e6, "recipient gets the rest");
    }

    /// @dev setParams 只有 owner 能呼叫，而且參數有上限（稅率 < 0.2%、單筆稅 < 50 USDT）。
    function test_usdtStyle_setParamsIsOwnerOnlyAndBounded() public {
        vm.prank(alice);
        vm.expectRevert(bytes("USDTMock: caller is not the owner"));
        usdt.setParams(10, 20);

        vm.startPrank(owner);

        vm.expectRevert(bytes("USDTMock: basis points too high"));
        usdt.setParams(20, 20);

        vm.expectRevert(bytes("USDTMock: max fee too high"));
        usdt.setParams(10, 50);

        vm.stopPrank();

        assertEq(usdt.basisPointsRate(), 0, "params unchanged");
        assertEq(usdt.maximumFee(), 0, "params unchanged");
    }

    // ====================================================================
    // 陷阱四：黑名單與 pause
    // ====================================================================

    /// @dev 忠於原版：黑名單只擋付款方（transfer 檢查 msg.sender、transferFrom 檢查 _from），
    ///      收款方是不是黑名單完全不檢查。所以往被凍結的位址付款會成功，錢卻再也拿不回來。
    function test_usdtStyle_blackListBlocksSenderOnly() public {
        vm.prank(owner);
        usdt.addBlackList(bob);

        // bob 轉不出去了
        vm.prank(bob);
        vm.expectRevert(bytes("USDTMock: sender is blacklisted"));
        usdt.transfer(alice, 1e6);

        // 但別人還是可以把錢轉進 bob 的帳上——這筆錢等於直接進了黑洞
        usdt.mint(alice, 10e6);
        vm.prank(alice);
        usdt.transfer(bob, 10e6);
        assertEq(usdt.balanceOf(bob), BOB_USDT + 10e6, "recipient blacklist is not checked");

        // transferFrom 檢查的是 _from，所以 bob 授權出去的額度也一起凍結
        vm.prank(bob);
        usdt.approve(alice, 5e6);
        vm.prank(alice);
        vm.expectRevert(bytes("USDTMock: from is blacklisted"));
        usdt.transferFrom(bob, alice, 5e6);

        // 解除黑名單之後恢復正常
        vm.prank(owner);
        usdt.removeBlackList(bob);
        vm.prank(bob);
        usdt.transfer(alice, 1e6);
        assertEq(usdt.balanceOf(alice), 1e6, "transfers work again after removal");
    }

    /// @dev owner 可以把黑名單位址的餘額直接歸零，totalSupply 同步減少。
    ///      對結算系統來說：帳上記著的餘額，可能在沒有任何交易的情況下憑空消失。
    function test_usdtStyle_destroyBlackFundsBurnsBalanceAndSupply() public {
        uint256 supplyBefore = usdt.totalSupply();

        // 還沒列入黑名單就不能銷毀
        vm.prank(owner);
        vm.expectRevert(bytes("USDTMock: user is not blacklisted"));
        usdt.destroyBlackFunds(bob);

        vm.startPrank(owner);
        usdt.addBlackList(bob);
        usdt.destroyBlackFunds(bob);
        vm.stopPrank();

        assertEq(usdt.balanceOf(bob), 0, "balance wiped");
        assertEq(usdt.totalSupply(), supplyBefore - BOB_USDT, "supply reduced by the same amount");
    }

    /// @dev pause 期間所有轉帳一律 revert，unpause 之後恢復。
    ///      relayer 必須把這種 revert 當成「暫時性失敗、之後重試」，而不是「這筆訂單失敗」。
    function test_usdtStyle_pauseBlocksAllTransfers() public {
        vm.prank(owner);
        usdt.pause();
        assertTrue(usdt.paused(), "paused");

        vm.prank(bob);
        vm.expectRevert(bytes("USDTMock: paused"));
        usdt.transfer(alice, 1e6);

        vm.prank(bob);
        vm.expectRevert(bytes("USDTMock: paused"));
        usdt.transferFrom(bob, alice, 1e6);

        vm.prank(owner);
        usdt.unpause();

        vm.prank(bob);
        usdt.transfer(alice, 1e6);
        assertEq(usdt.balanceOf(alice), 1e6, "transfers resume after unpause");
    }

    // ====================================================================
    // 陷阱五：失敗不 revert，只回傳 false
    // ====================================================================

    /// @dev 幽靈支付：alice 沒有任何餘額，轉帳卻「成功」了。
    ///      交易上鏈、receipt.status == 1、gas 照扣，但 bob 一毛錢都沒收到。
    ///      只有真的去解碼回傳值，才看得到那個 false。
    function test_noRevertToken_silentFailureWhenReturnIgnored() public {
        assertEq(noRevert.balanceOf(alice), 0, "alice starts with nothing");

        // 情境一：低階 call 只看 success —— 大量真實世界的整合程式碼就是這樣寫的
        vm.prank(alice);
        (bool success, bytes memory returnData) =
            address(noRevert).call(abi.encodeWithSelector(IERC20.transfer.selector, bob, 100e18));

        assertTrue(success, "the call itself succeeds: the token returns false instead of reverting");
        assertEq(noRevert.balanceOf(bob), 0, "but nothing moved: this is the phantom payment");
        assertEq(noRevert.totalSupply(), 0, "no supply change either");

        // 情境二：同一次呼叫，正確解碼回傳值就會看到 false
        assertEq(returnData.length, 32, "the token does return a bool, we just ignored it");
        assertFalse(abi.decode(returnData, (bool)), "decoded return value is false");

        // 用高階呼叫也一樣：不 revert，只是回傳 false
        vm.prank(alice);
        assertFalse(noRevert.transfer(bob, 100e18), "high level call returns false as well");
    }

    /// @dev 授權不足時同樣只回傳 false，不 revert。
    function test_noRevertToken_transferFromReturnsFalseWithoutAllowance() public {
        noRevert.mint(alice, 100e18);

        vm.prank(bob);
        bool success = noRevert.transferFrom(alice, bob, 10e18);

        assertFalse(success, "no allowance: returns false");
        assertEq(noRevert.balanceOf(alice), 100e18, "balances untouched");
        assertEq(noRevert.balanceOf(bob), 0, "balances untouched");
    }

    // ====================================================================
    // 陷阱六：轉帳抽稅
    // ====================================================================

    /// @dev 入帳金額 = 轉出金額 - fee。
    ///      任何「先記帳 amount、再轉帳 amount」的結算流程都會產生對不上的差額；
    ///      正確作法是用轉帳前後的餘額差，而不是相信呼叫時傳進去的數字。
    function test_feeOnTransfer_recipientReceivesLessThanSent() public {
        feeToken.mint(alice, 100e18);

        uint256 balanceBefore = feeToken.balanceOf(bob);

        vm.prank(alice);
        bool success = feeToken.transfer(bob, 100e18);

        uint256 received = feeToken.balanceOf(bob) - balanceBefore;

        assertTrue(success, "fee-on-transfer tokens still return true");
        assertEq(received, 99e18, "recipient receives amount minus 1% fee");
        assertEq(feeToken.balanceOf(owner), 1e18, "fee recipient collects the difference");
        assertEq(feeToken.balanceOf(alice), 0, "sender is debited the full amount");
        assertLt(received, 100e18, "received < sent");
    }

    /// @dev transferFrom 一樣會抽稅：allowance 扣的是「轉出金額」，收款方拿到的更少。
    function test_feeOnTransfer_transferFromAlsoTakesFee() public {
        feeToken.mint(alice, 100e18);

        vm.prank(alice);
        feeToken.approve(bob, 100e18);

        vm.prank(bob);
        feeToken.transferFrom(alice, bob, 50e18);

        assertEq(feeToken.allowance(alice, bob), 50e18, "allowance is spent at the gross amount");
        assertEq(feeToken.balanceOf(bob), 49.5e18, "recipient receives the net amount");
        assertEq(feeToken.balanceOf(owner), 0.5e18, "fee recipient collects the difference");
    }

    // ====================================================================
    // Fuzz：金額守恆
    // ====================================================================

    /// @dev 不管金額多少、稅有沒有封頂，錢都不會憑空消失或產生：
    ///      sender + recipient + owner 三方餘額總和永遠不變，而且收款方入帳不會超過轉出金額。
    ///      這條不變量是後面所有對帳邏輯的基礎——差額只會跑到 owner 手上，不會消失。
    function testFuzz_usdtStyle_amountIsConserved(uint256 amount) public {
        // 先把稅打開，讓 fuzz 同時覆蓋「按比例抽稅」與「被 maximumFee 封頂」兩種路徑
        vm.prank(owner);
        usdt.setParams(10, 20);

        amount = bound(amount, 0, usdt.balanceOf(bob));

        uint256 aliceBefore = usdt.balanceOf(alice);
        uint256 totalBefore = usdt.balanceOf(bob) + aliceBefore + usdt.balanceOf(owner);

        vm.prank(bob);
        usdt.transfer(alice, amount);

        uint256 received = usdt.balanceOf(alice) - aliceBefore;
        uint256 totalAfter = usdt.balanceOf(bob) + usdt.balanceOf(alice) + usdt.balanceOf(owner);

        assertEq(totalAfter, totalBefore, "money is neither created nor destroyed");
        assertLe(received, amount, "the recipient never receives more than was sent");
        assertEq(usdt.totalSupply(), BOB_USDT, "supply is untouched by transfers");
    }
}
