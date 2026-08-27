// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Test} from "forge-std/Test.sol";

import {Settlement} from "../src/Settlement.sol";
import {Permit2Mock} from "./mocks/Permit2Mock.sol";
import {ERC20Mock} from "../src/mocks/ERC20Mock.sol";
import {FeeOnTransferERC20Mock} from "../src/mocks/FeeOnTransferERC20Mock.sol";
import {NoRevertERC20Mock} from "../src/mocks/NoRevertERC20Mock.sol";
import {USDTMock} from "../src/mocks/USDTMock.sol";

/// @notice 一顆在批次搬錢途中重入 pay() 的惡意 token。
/// @dev 它是專門打 settleBatch() 的攻擊者，所以住在這個測試檔裡、不進 src/mocks/ 的
///      token 動物園，也不跟其他測試檔的攻擊者共用——一個時間點一顆，共用會讓測試
///      讀不出它在釘什麼。它打的時間點是「批次正在搬某一項的錢」：那一項的 ref 已經被
///      _reserve 占走，重入進來拿同一個 ref 走 pay()（唯一不用名單的入口），撞到的
///      必須是「ref already paid」。
contract BatchReentrantToken {
    Settlement private immutable settlement;
    bytes32 private ref;

    /// @notice 重入的那一次呼叫有沒有被擋下。
    bool public reentryBlocked;
    bool private reentered;

    mapping(address => uint256) private balances;

    constructor(Settlement settlement_) {
        settlement = settlement_;
    }

    /// @notice 佈置攻擊目標：重入時要重放的 ref。
    function arm(bytes32 ref_) external {
        ref = ref_;
    }

    function balanceOf(address account) external view returns (uint256) {
        return balances[account];
    }

    function transferFrom(address, address to, uint256 value) external returns (bool) {
        if (!reentered) {
            reentered = true;
            // 批次的第一項還在搬錢的半路上殺回來：ref 在搬錢之前就被占走了，
            // 這一次重放必須被擋下，不然同一筆付款在同一筆交易裡走完兩次。
            try settlement.pay(address(this), address(0xdead), value, ref) {
            // 重入沒被擋下——外面的測試會從 reentryBlocked 看出來
            }
            catch {
                reentryBlocked = true;
            }
        }
        balances[to] += value;
        return true;
    }

    function transfer(address, uint256) external pure returns (bool) {
        return true;
    }
}

/// @title SettlementBatchTest
/// @notice 把 settleBatch() 釘死：一筆交易結一批付款，每一項保有單筆付款的完整身分。
/// @dev 這些測試同時是批次的行為規格：每一項各發自己的 Paid、各占自己的 ref（批次內
///      重複的 ref 也擋）、跟另外三個當場結清入口與託管共用同一道 replay 防護；
///      中間任何一項失敗整批回滾，沒有半筆錢動過、也沒有任何 ref 被占走；
///      錢逐筆從 payer 直達 merchant，合約帳上一毛不留。
contract SettlementBatchTest is Test {
    address internal owner;
    address internal relayer;
    address internal payer;
    address internal merchant1;
    address internal merchant2;
    address internal merchant3;
    address internal outsider;
    address internal feeRecipient;

    Settlement internal settlement;
    ERC20Mock internal usdc;

    /// @dev 6 位小數：100e6 就是 100 USDC。三筆金額刻意不同，帳一對就知道有沒有搬錯人。
    uint256 internal constant AMOUNT1 = 100e6;
    uint256 internal constant AMOUNT2 = 250e6;
    uint256 internal constant AMOUNT3 = 40e6;
    uint256 internal constant PAYER_SEED = 1_000_000e6;
    bytes32 internal constant REF1 = keccak256("day-20/ref-1");
    bytes32 internal constant REF2 = keccak256("day-20/ref-2");
    bytes32 internal constant REF3 = keccak256("day-20/ref-3");

    function setUp() public {
        owner = makeAddr("owner");
        relayer = makeAddr("relayer");
        payer = makeAddr("payer");
        merchant1 = makeAddr("merchant1");
        merchant2 = makeAddr("merchant2");
        merchant3 = makeAddr("merchant3");
        outsider = makeAddr("outsider");
        feeRecipient = makeAddr("feeRecipient");

        // 這一檔測的是批次入口，Permit2 那條路徑由 SettlementPermit2.t.sol 負責，
        // 這裡只是給 constructor 一個真的有 code 的位址。
        Permit2Mock permit2 = new Permit2Mock();
        vm.prank(owner);
        settlement = new Settlement(address(permit2));
        vm.prank(owner);
        settlement.setRelayer(relayer, true);

        usdc = new ERC20Mock("USD Coin (mock)", "USDC", 6);
        usdc.mint(payer, PAYER_SEED);

        // 批次走的還是 allowance：這一檔的重點不在授權方式，直接給滿。
        vm.prank(payer);
        usdc.approve(address(settlement), type(uint256).max);
    }

    /// @dev 三個 merchant、三筆金額、三個 ref 的標準批次，後面的測試都從它出發。
    function threeItems() internal view returns (Settlement.Payout[] memory items) {
        items = new Settlement.Payout[](3);
        items[0] = Settlement.Payout({merchant: merchant1, amount: AMOUNT1, ref: REF1});
        items[1] = Settlement.Payout({merchant: merchant2, amount: AMOUNT2, ref: REF2});
        items[2] = Settlement.Payout({merchant: merchant3, amount: AMOUNT3, ref: REF3});
    }

    // ====================================================================
    // 一筆交易、N 筆付款
    // ====================================================================

    /// @dev 今天的論點本體：一筆交易裡三個 merchant 各收到自己的錢、三個 ref 各發一個
    ///      Paid event。對 listener 來說這跟三筆單獨的 settle() 沒有差別。
    function test_settleBatch_paysEveryItemInOneTransaction() public {
        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Paid(REF1, payer, merchant1, address(usdc), AMOUNT1, relayer);
        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Paid(REF2, payer, merchant2, address(usdc), AMOUNT2, relayer);
        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Paid(REF3, payer, merchant3, address(usdc), AMOUNT3, relayer);

        vm.prank(relayer);
        settlement.settleBatch(address(usdc), payer, threeItems());

        assertEq(usdc.balanceOf(merchant1), AMOUNT1, "merchant1 gets item 1");
        assertEq(usdc.balanceOf(merchant2), AMOUNT2, "merchant2 gets item 2");
        assertEq(usdc.balanceOf(merchant3), AMOUNT3, "merchant3 gets item 3");
        assertEq(usdc.balanceOf(payer), PAYER_SEED - AMOUNT1 - AMOUNT2 - AMOUNT3, "the payer pays the sum");
        assertEq(usdc.balanceOf(address(settlement)), 0, "the money never rests in the contract");
        assertTrue(settlement.paid(REF1), "ref 1 is taken");
        assertTrue(settlement.paid(REF2), "ref 2 is taken");
        assertTrue(settlement.paid(REF3), "ref 3 is taken");
    }

    /// @dev 一項的批次跟一筆 settle() 等價：同一個 event、同一種效果。批次沒有自己的
    ///      語義，它只是把 N 次 _settle 裝進同一筆交易。
    function test_settleBatch_aSingleItemBehavesLikeSettle() public {
        Settlement.Payout[] memory items = new Settlement.Payout[](1);
        items[0] = Settlement.Payout({merchant: merchant1, amount: AMOUNT1, ref: REF1});

        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Paid(REF1, payer, merchant1, address(usdc), AMOUNT1, relayer);

        vm.prank(relayer);
        settlement.settleBatch(address(usdc), payer, items);

        assertEq(usdc.balanceOf(merchant1), AMOUNT1, "one item, one payment");
    }

    /// @dev 批次動用的是 payer 的 allowance，入口跟 settle() 一樣要有名單。
    function test_settleBatch_rejectsCallerOutsideRelayerSet() public {
        vm.prank(outsider);
        vm.expectRevert(bytes("Settlement: caller is not a relayer"));
        settlement.settleBatch(address(usdc), payer, threeItems());
    }

    /// @dev 空批次只會是鏈下組批的 bug：一筆什麼都不做的交易不該安靜地成功。
    function test_settleBatch_rejectsAnEmptyBatch() public {
        Settlement.Payout[] memory items = new Settlement.Payout[](0);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: the batch is empty"));
        settlement.settleBatch(address(usdc), payer, items);
    }

    /// @dev 每一項走的是同一條 _settle，所以單筆付款的參數檢查一條不少；
    ///      金額為零這一項代表全組（零 merchant、零 ref 同一個把關）。
    function test_settleBatch_itemsPassTheSameChecksAsSettle() public {
        Settlement.Payout[] memory items = threeItems();
        items[1].amount = 0;

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: amount is zero"));
        settlement.settleBatch(address(usdc), payer, items);
    }

    // ====================================================================
    // replay 防護：批次內、批次外都是同一道
    // ====================================================================

    /// @dev 批次裡重複的 ref 就是同一筆付款出現兩次，第二次撞到的是同一句
    ///      「ref already paid」；而且整批回滾，第一次出現的那一項也不算數。
    function test_settleBatch_duplicateRefInsideTheBatchRevertsTheWholeBatch() public {
        Settlement.Payout[] memory items = threeItems();
        items[2].ref = REF1;

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.settleBatch(address(usdc), payer, items);

        assertEq(usdc.balanceOf(payer), PAYER_SEED, "nothing moved");
        assertFalse(settlement.paid(REF1), "a reverted batch burns no ref");
    }

    /// @dev replay 防護是整份合約共用的，兩個方向都要擋：當場結清用掉的 ref 進不了批次，
    ///      批次占走的 ref 也開不了託管。
    function test_settleBatch_sharesTheReplayGuardWithTheOtherDoors() public {
        vm.prank(relayer);
        settlement.settle(address(usdc), payer, merchant1, AMOUNT1, REF1);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.settleBatch(address(usdc), payer, threeItems());

        Settlement.Payout[] memory rest = new Settlement.Payout[](2);
        rest[0] = Settlement.Payout({merchant: merchant2, amount: AMOUNT2, ref: REF2});
        rest[1] = Settlement.Payout({merchant: merchant3, amount: AMOUNT3, ref: REF3});
        vm.prank(relayer);
        settlement.settleBatch(address(usdc), payer, rest);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.hold(
            address(usdc), payer, merchant2, AMOUNT2, 1e6, feeRecipient, REF2, uint64(block.timestamp + 1 days)
        );
    }

    /// @dev 釘死「每一項的 ref 都在搬錢之前占走」的順序：搬錢途中重入進來、拿同一個 ref
    ///      走 pay()，撞到的必須是拒絕；批次本身照常走完。
    function test_settleBatch_reentrantTokenCannotReplayABatchItem() public {
        BatchReentrantToken evil = new BatchReentrantToken(settlement);
        evil.arm(REF1);

        vm.prank(relayer);
        settlement.settleBatch(address(evil), payer, threeItems());

        assertTrue(evil.reentryBlocked(), "the reentrant replay must be rejected");
        assertTrue(settlement.paid(REF1), "the batch itself completes");
        assertTrue(settlement.paid(REF3), "all the way to the last item");
    }

    // ====================================================================
    // 一項失敗，整批回滾
    // ====================================================================

    /// @dev 原子性的代表案例：payer 的餘額只夠付到第二項，第三項回傳 false，
    ///      SafeTransfer 把它變成明確的 revert，前兩項已經搬走的錢也跟著回來。
    function test_settleBatch_aFalseReturnMidBatchRollsBackEverything() public {
        NoRevertERC20Mock quiet = new NoRevertERC20Mock("No Revert USD", "NRUSD", 6);
        quiet.mint(payer, AMOUNT1 + AMOUNT2);
        vm.prank(payer);
        quiet.approve(address(settlement), type(uint256).max);

        vm.prank(relayer);
        vm.expectRevert(bytes("SafeTransfer: transfer returned false"));
        settlement.settleBatch(address(quiet), payer, threeItems());

        assertEq(quiet.balanceOf(merchant1), 0, "item 1 is rolled back too");
        assertEq(quiet.balanceOf(payer), AMOUNT1 + AMOUNT2, "the payer keeps everything");
        assertFalse(settlement.paid(REF1), "no ref is burned by a failed batch");
        assertFalse(settlement.paid(REF3), "not even the item that never ran");
    }

    /// @dev 出金用的是 SafeTransfer：USDT 這種不回傳值的 token，一整批也要走得完，
    ///      不能死在任何一項的解碼上。
    function test_settleBatch_usdtStyleTokenGoesThroughTheWholeBatch() public {
        USDTMock usdt = new USDTMock();
        usdt.mint(payer, PAYER_SEED);
        vm.prank(payer);
        usdt.approve(address(settlement), type(uint256).max);

        vm.prank(relayer);
        settlement.settleBatch(address(usdt), payer, threeItems());

        assertEq(usdt.balanceOf(merchant1), AMOUNT1, "tether-shaped tokens disperse too");
        assertEq(usdt.balanceOf(merchant3), AMOUNT3, "all the way to the last item");
    }

    /// @dev 批次不量實收，跟單筆 settle() 同一條規則：fee-on-transfer 的短少發生在
    ///      payer 到 merchant 那一段，event 記請款金額，缺口留給鏈下對帳。
    function test_settleBatch_feeOnTransferShortfallIsLeftToTheLedger() public {
        // 1% 的常駐轉帳稅
        FeeOnTransferERC20Mock fot = new FeeOnTransferERC20Mock("Fee On Transfer USD", "FOTUSD", 6, 100, feeRecipient);
        fot.mint(payer, PAYER_SEED);
        vm.prank(payer);
        fot.approve(address(settlement), type(uint256).max);

        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Paid(REF1, payer, merchant1, address(fot), AMOUNT1, relayer);

        vm.prank(relayer);
        settlement.settleBatch(address(fot), payer, threeItems());

        assertEq(fot.balanceOf(merchant1), AMOUNT1 - AMOUNT1 / 100, "the merchant arrives short");
        assertEq(fot.balanceOf(address(settlement)), 0, "the shortfall is not the contract's problem");
    }

    // ====================================================================
    // 規模與守恆
    // ====================================================================

    /// @dev 一批兩百個 merchant 塞進同一筆交易：批次的上限是區塊的 gas 上限，不是合約。
    ///      這條測試印出來的 gas 就是文章裡引用的實跑數字。
    function test_settleBatch_twoHundredMerchantsFitInOneTransaction() public {
        uint256 n = 200;
        Settlement.Payout[] memory items = new Settlement.Payout[](n);
        for (uint256 i = 0; i < n; i++) {
            items[i] = Settlement.Payout({
                merchant: address(uint160(0x1000 + i)), amount: 1e6, ref: keccak256(abi.encodePacked("day-20/bulk-", i))
            });
        }

        vm.prank(relayer);
        settlement.settleBatch(address(usdc), payer, items);

        assertEq(usdc.balanceOf(payer), PAYER_SEED - n * 1e6, "the payer pays all two hundred");
        assertEq(usdc.balanceOf(address(uint160(0x1000))), 1e6, "the first merchant is paid");
        assertEq(usdc.balanceOf(address(uint160(0x1000 + n - 1))), 1e6, "so is the last");
        assertTrue(settlement.paid(items[n - 1].ref), "and every ref is taken");
    }

    /// @dev 任意三筆金額下守恆：merchant 們拿到的剛好是 payer 付出的，合約一毛不留、
    ///      token 總量不變。
    function testFuzz_settleBatch_conservesMoney(uint256 a1, uint256 a2, uint256 a3) public {
        Settlement.Payout[] memory items = threeItems();
        items[0].amount = bound(a1, 1, PAYER_SEED / 3);
        items[1].amount = bound(a2, 1, PAYER_SEED / 3);
        items[2].amount = bound(a3, 1, PAYER_SEED / 3);

        vm.prank(relayer);
        settlement.settleBatch(address(usdc), payer, items);

        uint256 paidOut = items[0].amount + items[1].amount + items[2].amount;
        assertEq(usdc.balanceOf(payer), PAYER_SEED - paidOut);
        assertEq(usdc.balanceOf(merchant1), items[0].amount);
        assertEq(usdc.balanceOf(merchant2), items[1].amount);
        assertEq(usdc.balanceOf(merchant3), items[2].amount);
        assertEq(usdc.balanceOf(address(settlement)), 0, "the contract keeps nothing");
        assertEq(usdc.totalSupply(), PAYER_SEED, "settlement mints and burns nothing");
    }
}
