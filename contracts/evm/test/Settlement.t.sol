// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Test} from "forge-std/Test.sol";

import {Settlement} from "../src/Settlement.sol";
import {ERC20Mock} from "../src/mocks/ERC20Mock.sol";
import {FeeOnTransferERC20Mock} from "../src/mocks/FeeOnTransferERC20Mock.sol";
import {NoRevertERC20Mock} from "../src/mocks/NoRevertERC20Mock.sol";
import {USDTMock} from "../src/mocks/USDTMock.sol";

/// @notice 一顆會重入的惡意 token：transferFrom 執行到一半時，回頭拿同一個 ref 再呼叫一次結算合約。
/// @dev 它不是 Token Zoo 的成員——動物園收的是真實世界的 token 例外，這顆是專門打 Settlement 的攻擊者，
///      所以住在測試檔裡。它完全不記帳，transferFrom 永遠成功，重點只在呼叫順序。
contract ReentrantToken {
    Settlement private immutable settlement;
    address private merchant;
    bytes32 private ref;

    /// @notice 重入的那一次呼叫有沒有被擋下。
    bool public reentryBlocked;
    /// @notice transferFrom 完整走完的次數。重入成功的話會是 2。
    uint256 public transfers;
    bool private reentered;

    constructor(Settlement settlement_) {
        settlement = settlement_;
    }

    /// @notice 佈置攻擊目標：重入時要打的 merchant 與 ref。
    function arm(address merchant_, bytes32 ref_) external {
        merchant = merchant_;
        ref = ref_;
    }

    function transferFrom(address, address, uint256) external returns (bool) {
        if (!reentered) {
            reentered = true;
            // 在 ref 標記與 event 之間殺回來：如果合約是「先搬錢、再占 ref」，這一次呼叫會成功，
            // 同一個 ref 就走完了兩次。
            try settlement.pay(address(this), merchant, 1, ref) {
            // 重入沒被擋下——外面的測試會從 transfers == 2 看出來
            }
            catch {
                reentryBlocked = true;
            }
        }
        transfers += 1;
        return true;
    }
}

/// @title SettlementTest
/// @notice 把 Settlement 的兩個入口、replay 防護與「穿過它的四類例外」全部釘死。
/// @dev 這些測試同時是這份合約的行為規格：哪些失敗是安全的（revert、什麼都沒動）、
///      哪些成功是帶著已知缺陷的（fee-on-transfer 的實收短少留給鏈下）。
contract SettlementTest is Test {
    address internal owner;
    address internal relayer;
    address internal payer;
    address internal merchant;
    address internal outsider;
    address internal feeCollector;

    Settlement internal settlement;
    ERC20Mock internal usdc;

    /// @dev 6 位小數：100e6 就是 100 USDC。
    uint256 internal constant AMOUNT = 100e6;
    uint256 internal constant PAYER_SEED = 1_000_000e6;
    bytes32 internal constant REF = keccak256("day-16/ref-1");

    function setUp() public {
        owner = makeAddr("owner");
        relayer = makeAddr("relayer");
        payer = makeAddr("payer");
        merchant = makeAddr("merchant");
        outsider = makeAddr("outsider");
        feeCollector = makeAddr("feeCollector");

        vm.prank(owner);
        settlement = new Settlement();
        vm.prank(owner);
        settlement.setRelayer(relayer, true);

        usdc = new ERC20Mock("USD Coin (mock)", "USDC", 6);
        usdc.mint(payer, PAYER_SEED);
    }

    /// @dev 幫 payer 給結算合約 allowance 的 helper。pull 與 push 都要走這一步。
    function approveSettlement(address token, uint256 amount) internal {
        vm.prank(payer);
        ERC20Mock(token).approve(address(settlement), amount);
    }

    // ====================================================================
    // pull 支付流：relayer 發起，動用 payer 事先給的 allowance
    // ====================================================================

    /// @dev pull 的快樂路徑：relayer 簽名發起，錢從 payer 直達 merchant，event 帶著 ref、
    ///      executor 是 relayer。這就是整條 relayer pipeline 最後要落地的那一步。
    function test_settle_relayerMovesPayerMoney() public {
        approveSettlement(address(usdc), AMOUNT);

        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Paid(REF, payer, merchant, address(usdc), AMOUNT, relayer);

        vm.prank(relayer);
        settlement.settle(address(usdc), payer, merchant, AMOUNT, REF);

        assertEq(usdc.balanceOf(payer), PAYER_SEED - AMOUNT, "payer pays");
        assertEq(usdc.balanceOf(merchant), AMOUNT, "merchant receives");
        assertEq(usdc.balanceOf(address(settlement)), 0, "the contract never holds funds");
        assertTrue(settlement.paid(REF), "ref is marked paid");
    }

    /// @dev 名單檢查防的是「任何人都能替 payer 花 allowance」：settle 的收款人是呼叫端指定的，
    ///      沒有這道檢查，任何人都能把 payer 授權的錢搬去任意地址。
    function test_settle_rejectsCallerOutsideRelayerSet() public {
        approveSettlement(address(usdc), AMOUNT);

        vm.prank(outsider);
        vm.expectRevert(bytes("Settlement: caller is not a relayer"));
        settlement.settle(address(usdc), payer, outsider, AMOUNT, REF);
    }

    /// @dev relayer 名單只有 owner 動得了，而且變更要留下 event，鏈下才能稽核名單的歷史。
    function test_setRelayer_isOwnerOnly() public {
        vm.prank(outsider);
        vm.expectRevert(bytes("Settlement: caller is not the owner"));
        settlement.setRelayer(outsider, true);

        vm.expectEmit(true, false, false, true, address(settlement));
        emit Settlement.RelayerSet(outsider, true);
        vm.prank(owner);
        settlement.setRelayer(outsider, true);
        assertTrue(settlement.isRelayer(outsider));
    }

    // ====================================================================
    // push 支付流：payer 自己發起、自己付 gas
    // ====================================================================

    /// @dev push 的快樂路徑：不需要在任何名單上，payer 搬的是自己的錢。
    ///      event 跟 pull 長得一模一樣，只有 executor 換成 payer 本人。
    function test_pay_movesMoneyAndEmitsPaid() public {
        approveSettlement(address(usdc), AMOUNT);

        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Paid(REF, payer, merchant, address(usdc), AMOUNT, payer);

        vm.prank(payer);
        settlement.pay(address(usdc), merchant, AMOUNT, REF);

        assertEq(usdc.balanceOf(merchant), AMOUNT, "merchant receives");
    }

    /// @dev push 也繞不開 approve：錢要繞經合約，ref 才有地方上鏈。
    ///      這是 EVM 沒有 memo 欄位的代價——payer 得先多發一筆交易。
    function test_pay_requiresPriorAllowance() public {
        vm.prank(payer);
        vm.expectRevert(bytes("ERC20Mock: insufficient allowance"));
        settlement.pay(address(usdc), merchant, AMOUNT, REF);
    }

    // ====================================================================
    // replay 防護：同一個 ref 只結算一次
    // ====================================================================

    /// @dev 「錢只動一次」在鏈上的那一格 storage：第二次拿同一個 ref 來，不管金額、
    ///      token、收款人是什麼，一律拒絕。
    function test_settle_sameRefPaysOnlyOnce() public {
        approveSettlement(address(usdc), AMOUNT * 2);

        vm.prank(relayer);
        settlement.settle(address(usdc), payer, merchant, AMOUNT, REF);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.settle(address(usdc), payer, merchant, AMOUNT, REF);

        assertEq(usdc.balanceOf(merchant), AMOUNT, "money moved exactly once");
    }

    /// @dev 兩個入口共用同一條搬錢路徑，所以 replay 防護跨支付流生效：
    ///      pull 結算過的付款，payer 自己再 push 一次也進不來。
    function test_pullAndPushShareTheReplayGuard() public {
        approveSettlement(address(usdc), AMOUNT * 2);

        vm.prank(relayer);
        settlement.settle(address(usdc), payer, merchant, AMOUNT, REF);

        vm.prank(payer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.pay(address(usdc), merchant, AMOUNT, REF);
    }

    /// @dev 失敗的嘗試不會把 ref 燒掉：transferFrom 一 revert，整筆交易連同 ref 的標記
    ///      一起回滾，補救之後同一個 ref 還走得完。這是鏈上交易原子性送的，不用自己寫補償。
    function test_settle_failedTransferDoesNotBurnTheRef() public {
        // 還沒 approve，第一次 settle 會死在 transferFrom
        vm.prank(relayer);
        vm.expectRevert(bytes("ERC20Mock: insufficient allowance"));
        settlement.settle(address(usdc), payer, merchant, AMOUNT, REF);
        assertFalse(settlement.paid(REF), "a failed attempt leaves no mark");

        approveSettlement(address(usdc), AMOUNT);
        vm.prank(relayer);
        settlement.settle(address(usdc), payer, merchant, AMOUNT, REF);
        assertEq(usdc.balanceOf(merchant), AMOUNT, "the same ref succeeds after the fix");
    }

    /// @dev 釘死「先占 ref、再搬錢」的順序：一顆會重入的 token 在 transferFrom 裡
    ///      拿同一個 ref 殺回來，撞到的必須是「ref already paid」。
    ///      順序反過來的話 transfers 會變成 2——同一筆付款走完兩次。
    function test_settle_reentrantTokenCannotReplayTheRef() public {
        ReentrantToken evil = new ReentrantToken(settlement);
        evil.arm(merchant, REF);

        vm.prank(relayer);
        settlement.settle(address(evil), payer, merchant, AMOUNT, REF);

        assertTrue(evil.reentryBlocked(), "the reentrant call must be rejected");
        assertEq(evil.transfers(), 1, "money moves exactly once");
    }

    // ====================================================================
    // 入口的防呆檢查
    // ====================================================================

    /// @dev 零值 ref 代表「還沒算」，任何地方看到都是 bug，合約直接拒收。
    function test_settle_zeroRefIsRejected() public {
        approveSettlement(address(usdc), AMOUNT);
        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref is zero"));
        settlement.settle(address(usdc), payer, merchant, AMOUNT, bytes32(0));
    }

    /// @dev 收款人是零地址等於把錢燒掉，而且很多 token（包括我們的 mock）不會擋。
    function test_settle_zeroMerchantIsRejected() public {
        approveSettlement(address(usdc), AMOUNT);
        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: merchant is the zero address"));
        settlement.settle(address(usdc), payer, address(0), AMOUNT, REF);
    }

    /// @dev 0 元的付款只會是 bug；EIP-20 要求 token 接受 0 元轉帳，所以要擋得在這裡擋。
    function test_settle_zeroAmountIsRejected() public {
        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: amount is zero"));
        settlement.settle(address(usdc), payer, merchant, 0, REF);
    }

    /// @dev token 位址上沒有程式碼的話整筆 revert：Solidity 對「預期有回傳值」的外部呼叫
    ///      會先檢查對方有沒有 code，對一個 EOA 呼叫 transferFrom 不會安靜地成功。
    function test_settle_revertsWhenTokenHasNoCode() public {
        vm.prank(relayer);
        vm.expectRevert();
        settlement.settle(makeAddr("not-a-token"), payer, merchant, AMOUNT, REF);
    }

    // ====================================================================
    // 四類例外穿過這份合約
    // ====================================================================

    /// @dev 「不會 revert 的」：token 用回傳 false 表達失敗，require(ok) 把它轉成明確的
    ///      revert。少了這一行，event 照發、錢沒動，就是 Day 2 的幽靈支付。
    function test_settle_noRevertTokenFalseIsCaught() public {
        NoRevertERC20Mock bad = new NoRevertERC20Mock("No Revert USD", "NRUSD", 18);
        // 刻意不給 allowance：這顆 token 會回傳 false 而不是 revert

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: transferFrom returned false"));
        settlement.settle(address(bad), payer, merchant, AMOUNT, REF);
        assertFalse(settlement.paid(REF), "nothing moved, nothing marked");
    }

    /// @dev 「會 revert 的」：USDT 的 transferFrom 沒有回傳值，標準介面在解碼那一步 revert，
    ///      連 USDT 內部已經更新的帳一起回滾。失敗是安全的，但也代表 USDT 今天完全過不了
    ///      這份合約——這就是標準介面的邊界。
    function test_settle_usdtRevertsThroughStandardInterface() public {
        vm.prank(owner);
        USDTMock usdt = new USDTMock();
        usdt.mint(payer, PAYER_SEED);
        vm.prank(payer);
        usdt.approve(address(settlement), AMOUNT);

        vm.prank(relayer);
        vm.expectRevert();
        settlement.settle(address(usdt), payer, merchant, AMOUNT, REF);

        assertEq(usdt.balanceOf(payer), PAYER_SEED, "the revert rolls USDT's move back too");
        assertFalse(settlement.paid(REF), "ref is not burned");
    }

    /// @dev 「金額對不上的」：fee-on-transfer 的 token 照樣放行，event 記的是請款金額，
    ///      merchant 實收短少。合約刻意不量實收——金額核對的判斷住在鏈下的 listener。
    function test_settle_feeOnTransferEmitsRequestedAmount() public {
        FeeOnTransferERC20Mock fot = new FeeOnTransferERC20Mock("Fee On Transfer USD", "FOTUSD", 6, 100, feeCollector);
        fot.mint(payer, PAYER_SEED);
        vm.prank(payer);
        fot.approve(address(settlement), AMOUNT);

        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Paid(REF, payer, merchant, address(fot), AMOUNT, relayer);

        vm.prank(relayer);
        settlement.settle(address(fot), payer, merchant, AMOUNT, REF);

        uint256 fee = AMOUNT / 100; // 1%
        assertEq(fot.balanceOf(merchant), AMOUNT - fee, "the merchant receives less than the event says");
        assertEq(fot.balanceOf(feeCollector), fee, "the tax went to the collector");
    }

    // ====================================================================
    // 守恆
    // ====================================================================

    /// @dev 任意金額下，錢從 payer 直達 merchant、一毛不多一毛不少，合約手上一毛不留。
    function testFuzz_settle_movesExactlyTheRequestedAmount(uint256 amount) public {
        amount = bound(amount, 1, PAYER_SEED);
        approveSettlement(address(usdc), amount);

        vm.prank(relayer);
        settlement.settle(address(usdc), payer, merchant, amount, REF);

        assertEq(usdc.balanceOf(payer), PAYER_SEED - amount);
        assertEq(usdc.balanceOf(merchant), amount);
        assertEq(usdc.balanceOf(address(settlement)), 0);
        assertEq(usdc.totalSupply(), PAYER_SEED, "settlement mints and burns nothing");
    }
}
