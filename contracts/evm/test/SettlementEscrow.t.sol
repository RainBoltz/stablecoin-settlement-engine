// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Test} from "forge-std/Test.sol";

import {Settlement} from "../src/Settlement.sol";
import {Permit2Mock} from "./mocks/Permit2Mock.sol";
import {ERC20Mock} from "../src/mocks/ERC20Mock.sol";
import {FeeOnTransferERC20Mock} from "../src/mocks/FeeOnTransferERC20Mock.sol";
import {USDTMock} from "../src/mocks/USDTMock.sol";

/// @notice 一顆在合約付錢出去時重入 release() 的惡意 token。
/// @dev 它是專門打 Settlement 託管路徑的攻擊者，所以住在這個測試檔裡、不進 src/mocks/ 的
///      token 動物園。它完全不記帳：transferFrom（入金）與 transfer（出金）都回報成功，balanceOf 只做到
///      能讓 hold() 的實收檢查通過的程度。重點只在呼叫順序——出金那一步殺回 release()，
///      如果 hold 記錄還在，同一筆託管就拆帳兩次。
contract ReleaseReentrantToken {
    Settlement private immutable settlement;
    bytes32 private ref;

    /// @notice 重入的那一次呼叫有沒有被擋下。
    bool public reentryBlocked;
    /// @notice 合約付錢出去的次數。一筆帶手續費的託管，正常的 release 剛好付兩次。
    uint256 public payouts;
    bool private reentered;

    mapping(address => uint256) private balances;

    constructor(Settlement settlement_) {
        settlement = settlement_;
    }

    /// @notice 佈置攻擊目標：重入時要打的 ref。
    function arm(bytes32 ref_) external {
        ref = ref_;
    }

    function balanceOf(address account) external view returns (uint256) {
        return balances[account];
    }

    function transferFrom(address, address to, uint256 value) external returns (bool) {
        balances[to] += value;
        return true;
    }

    function transfer(address, uint256) external returns (bool) {
        if (!reentered) {
            reentered = true;
            // 在記錄刪除與搬錢之間殺回來：如果合約是「先搬錢、再刪記錄」，這一次呼叫會成功，
            // 同一筆託管就拆帳兩次。
            try settlement.release(ref) {
            // 重入沒被擋下——外面的測試會從 payouts 的次數看出來
            }
            catch {
                reentryBlocked = true;
            }
        }
        payouts += 1;
        return true;
    }
}

/// @notice 一顆在入金途中就重入 release() 的惡意 token。
/// @dev 跟 ReleaseReentrantToken 一樣住在這個測試檔，打的是另一個時間點：hold() 收錢的
///      那一步。hold 記錄刻意寫在搬錢之後，所以這一刻的重入找不到任何可以 release 的
///      記錄；順序反過來的話，一筆錢還沒入帳的託管就能先被拆帳。
contract DepositReentrantToken {
    Settlement private immutable settlement;
    bytes32 private ref;

    /// @notice 重入的那一次呼叫有沒有被擋下。
    bool public reentryBlocked;
    bool private reentered;

    mapping(address => uint256) private balances;

    constructor(Settlement settlement_) {
        settlement = settlement_;
    }

    /// @notice 佈置攻擊目標：重入時要打的 ref。
    function arm(bytes32 ref_) external {
        ref = ref_;
    }

    function balanceOf(address account) external view returns (uint256) {
        return balances[account];
    }

    function transferFrom(address, address to, uint256 value) external returns (bool) {
        if (!reentered) {
            reentered = true;
            // 錢還在往合約裡搬的半路上殺回來：如果 hold 記錄已經寫好，這一次呼叫會成功，
            // 一筆還沒入帳的託管就被拆帳了。
            try settlement.release(ref) {
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

/// @title SettlementEscrowTest
/// @notice 把託管的三個函式釘死：hold 收錢、release 拆帳、refund 全額退回。
/// @dev 這些測試同時是託管的行為規格：錢在合約裡的期間誰能把它拿出去（release 只有
///      relayer；refund 是 relayer 隨時、payer 過了 refundAfter 之後、其他人永遠不行）、
///      手續費什麼時候才真的收到（結清那一刻）、以及一個 ref 被占用之後就不再回來
///      （refund 也不解鎖）。
contract SettlementEscrowTest is Test {
    address internal owner;
    address internal relayer;
    address internal payer;
    address internal merchant;
    address internal outsider;
    address internal feeRecipient;

    Settlement internal settlement;
    ERC20Mock internal usdc;

    /// @dev 6 位小數：100e6 就是 100 USDC，手續費收 1%。
    uint256 internal constant AMOUNT = 100e6;
    uint256 internal constant FEE = 1e6;
    uint256 internal constant PAYER_SEED = 1_000_000e6;
    bytes32 internal constant REF = keccak256("day-19/ref-1");
    uint64 internal refundAfter;

    function setUp() public {
        owner = makeAddr("owner");
        relayer = makeAddr("relayer");
        payer = makeAddr("payer");
        merchant = makeAddr("merchant");
        outsider = makeAddr("outsider");
        feeRecipient = makeAddr("feeRecipient");

        // 這一檔測的是託管的三個函式，Permit2 那條路徑由 SettlementPermit2.t.sol 負責，
        // 這裡只是給 constructor 一個真的有 code 的位址。
        Permit2Mock permit2 = new Permit2Mock();
        vm.prank(owner);
        settlement = new Settlement(address(permit2));
        vm.prank(owner);
        settlement.setRelayer(relayer, true);

        usdc = new ERC20Mock("USD Coin (mock)", "USDC", 6);
        usdc.mint(payer, PAYER_SEED);

        // 託管走的還是 allowance：這一檔的重點不在授權方式，直接給滿。
        vm.prank(payer);
        usdc.approve(address(settlement), type(uint256).max);

        refundAfter = uint64(block.timestamp + 3 days);
    }

    /// @dev 用預設參數開一筆託管的 helper，後面的測試都從它出發。
    function holdDefault() internal {
        vm.prank(relayer);
        settlement.hold(address(usdc), payer, merchant, AMOUNT, FEE, feeRecipient, REF, refundAfter);
    }

    // ====================================================================
    // hold：錢進合約，ref 當場占用
    // ====================================================================

    /// @dev 今天的論點本體：錢離開 payer 之後沒有直達 merchant，先住進合約；
    ///      hold 記錄與 paid 標記同時成立，fee 在這一刻就講定。
    function test_hold_movesTheMoneyIntoTheContract() public {
        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Held(REF, payer, merchant, address(usdc), AMOUNT, FEE, refundAfter);

        holdDefault();

        assertEq(usdc.balanceOf(payer), PAYER_SEED - AMOUNT, "payer pays up front");
        assertEq(usdc.balanceOf(address(settlement)), AMOUNT, "the contract holds the money");
        assertEq(usdc.balanceOf(merchant), 0, "the merchant has nothing yet");
        assertTrue(settlement.paid(REF), "ref is taken the moment the hold opens");

        (,,, uint256 heldAmount, uint256 heldFee,,) = settlement.holds(REF);
        assertEq(heldAmount, AMOUNT, "the record keeps the amount");
        assertEq(heldFee, FEE, "and the fee agreed up front");
    }

    /// @dev 託管動用的是 payer 給這份合約的 allowance，所以入口跟 settle() 一樣要有名單。
    function test_hold_rejectsCallerOutsideRelayerSet() public {
        vm.prank(outsider);
        vm.expectRevert(bytes("Settlement: caller is not a relayer"));
        settlement.hold(address(usdc), payer, merchant, AMOUNT, FEE, feeRecipient, REF, refundAfter);
    }

    /// @dev replay 防護是整份合約共用的：當場結清用掉的 ref 開不了託管，
    ///      託管占走的 ref 也擋得住 push 進來的重複付款。
    function test_hold_sharesTheReplayGuardWithTheSettleDoors() public {
        vm.prank(relayer);
        settlement.settle(address(usdc), payer, merchant, AMOUNT, REF);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.hold(address(usdc), payer, merchant, AMOUNT, FEE, feeRecipient, REF, refundAfter);

        bytes32 ref2 = keccak256("day-19/ref-2");
        vm.prank(relayer);
        settlement.hold(address(usdc), payer, merchant, AMOUNT, FEE, feeRecipient, ref2, refundAfter);

        vm.prank(payer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.pay(address(usdc), merchant, AMOUNT, ref2);
    }

    /// @dev 手續費是從 amount 裡拆出來的，吃掉整筆金額的 fee 只會是鏈下算錯了，直接拒收。
    function test_hold_feeMustBeLessThanTheAmount() public {
        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: fee is not less than the amount"));
        settlement.hold(address(usdc), payer, merchant, AMOUNT, AMOUNT, feeRecipient, REF, refundAfter);
    }

    /// @dev 要收費就要講清楚錢給誰：fee 大於零而 feeRecipient 是零地址，等於把手續費燒掉。
    function test_hold_feeNeedsARecipient() public {
        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: fee recipient is the zero address"));
        settlement.hold(address(usdc), payer, merchant, AMOUNT, FEE, address(0), REF, refundAfter);
    }

    /// @dev 已經過去的 refundAfter 會讓 payer 立刻就能退款，這筆託管形同虛設，只會是參數錯了。
    function test_hold_refundAfterMustBeInTheFuture() public {
        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: refundAfter is not in the future"));
        settlement.hold(address(usdc), payer, merchant, AMOUNT, FEE, feeRecipient, REF, uint64(block.timestamp));
    }

    /// @dev 託管的入金要一毛不差：fee-on-transfer 的 token 入帳短少，之後 release 或 refund
    ///      會因為餘額不足而卡死，所以在入口整筆拒收，ref 也不被燒掉。
    ///      這是本合約唯一量實收的地方——當場結清的三個入口照舊不量。
    function test_hold_shortDepositIsRejected() public {
        FeeOnTransferERC20Mock fot = new FeeOnTransferERC20Mock("Fee On Transfer USD", "FOTUSD", 6, 100, outsider);
        fot.mint(payer, PAYER_SEED);
        vm.prank(payer);
        fot.approve(address(settlement), AMOUNT);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: deposit arrived short"));
        settlement.hold(address(fot), payer, merchant, AMOUNT, FEE, feeRecipient, REF, refundAfter);

        assertFalse(settlement.paid(REF), "a rejected deposit does not burn the ref");
    }

    /// @dev 釘死「hold 記錄寫在搬錢之後」的順序：入金途中重入進來的 release 找不到
    ///      記錄，等錢真的入帳、記錄寫好，同一筆託管照常結得了帳。
    function test_hold_reentrantDepositFindsNoHoldToRelease() public {
        DepositReentrantToken evil = new DepositReentrantToken(settlement);
        evil.arm(REF);

        vm.prank(relayer);
        settlement.hold(address(evil), payer, merchant, AMOUNT, FEE, feeRecipient, REF, refundAfter);

        assertTrue(evil.reentryBlocked(), "the reentrant call must be rejected");
        (,,, uint256 heldAmount,,,) = settlement.holds(REF);
        assertEq(heldAmount, AMOUNT, "the hold record is written after the money arrived");
    }

    /// @dev 出金用的是 SafeTransfer 的 safeTransfer：USDT 這種不回傳值的 token，
    ///      整條 hold 加 release 的流程也要走得完，不能死在出金的解碼上。
    function test_hold_usdtStyleTokenGoesThroughTheWholeFlow() public {
        USDTMock usdt = new USDTMock();
        usdt.mint(payer, PAYER_SEED);
        vm.prank(payer);
        usdt.approve(address(settlement), AMOUNT);

        vm.prank(relayer);
        settlement.hold(address(usdt), payer, merchant, AMOUNT, FEE, feeRecipient, REF, refundAfter);
        vm.prank(relayer);
        settlement.release(REF);

        assertEq(usdt.balanceOf(merchant), AMOUNT - FEE, "the merchant is paid in tether-shaped tokens");
        assertEq(usdt.balanceOf(feeRecipient), FEE, "and the fee arrives too");
    }

    // ====================================================================
    // release：拆帳，手續費在這一刻才真的收到
    // ====================================================================

    /// @dev 拆帳照 hold 時講定的條件走：amount 減 fee 給 merchant，fee 給 feeRecipient，
    ///      合約歸零，記錄刪除。
    function test_release_splitsTheAmountBetweenMerchantAndFeeRecipient() public {
        holdDefault();

        vm.expectEmit(true, true, false, true, address(settlement));
        emit Settlement.Released(REF, merchant, address(usdc), AMOUNT - FEE, FEE, feeRecipient);

        vm.prank(relayer);
        settlement.release(REF);

        assertEq(usdc.balanceOf(merchant), AMOUNT - FEE, "the merchant receives the amount minus the fee");
        assertEq(usdc.balanceOf(feeRecipient), FEE, "the fee recipient receives the fee");
        assertEq(usdc.balanceOf(address(settlement)), 0, "the contract keeps nothing");

        (,,, uint256 heldAmount,,,) = settlement.holds(REF);
        assertEq(heldAmount, 0, "the hold record is gone");
    }

    /// @dev 免費的託管也要走得通：fee 為零時跳過第二筆出金，feeRecipient 可以不填。
    function test_release_zeroFeePaysTheMerchantAlone() public {
        vm.prank(relayer);
        settlement.hold(address(usdc), payer, merchant, AMOUNT, 0, address(0), REF, refundAfter);

        vm.prank(relayer);
        settlement.release(REF);

        assertEq(usdc.balanceOf(merchant), AMOUNT, "the merchant receives the full amount");
        assertEq(usdc.balanceOf(address(settlement)), 0, "the contract keeps nothing");
    }

    /// @dev 把錢從合約放出去的動作跟收進來一樣，只有名單上的 relayer 做得到。
    function test_release_rejectsCallerOutsideRelayerSet() public {
        holdDefault();

        vm.prank(outsider);
        vm.expectRevert(bytes("Settlement: caller is not a relayer"));
        settlement.release(REF);
    }

    /// @dev release 只認得還開著的託管：沒開過的 ref、已經結清的 ref、已經退回的 ref，
    ///      看到的都是同一個拒絕。
    function test_release_needsAnOpenHold() public {
        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: no hold for this ref"));
        settlement.release(REF);

        holdDefault();
        vm.prank(relayer);
        settlement.release(REF);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: no hold for this ref"));
        settlement.release(REF);
    }

    /// @dev 釘死「先刪記錄、再搬錢」的順序：一顆會重入的 token 在出金時殺回 release，
    ///      撞到的必須是「no hold for this ref」。順序反過來的話 payouts 會多出兩次。
    function test_release_reentrantTokenCannotDoubleRelease() public {
        ReleaseReentrantToken evil = new ReleaseReentrantToken(settlement);
        evil.arm(REF);

        vm.prank(relayer);
        settlement.hold(address(evil), payer, merchant, AMOUNT, FEE, feeRecipient, REF, refundAfter);

        vm.prank(relayer);
        settlement.release(REF);

        assertTrue(evil.reentryBlocked(), "the reentrant call must be rejected");
        assertEq(evil.payouts(), 2, "one payout to the merchant and one fee, nothing more");
    }

    // ====================================================================
    // refund：全額退回，refundAfter 之後不求人
    // ====================================================================

    /// @dev 手續費只在結清時收：退回去的託管一毛都不扣，fee 跟著本金一起回到 payer 身上。
    function test_refund_returnsTheFullAmountIncludingTheFee() public {
        holdDefault();

        vm.expectEmit(true, true, false, true, address(settlement));
        emit Settlement.Refunded(REF, payer, address(usdc), AMOUNT);

        vm.prank(relayer);
        settlement.refund(REF);

        assertEq(usdc.balanceOf(payer), PAYER_SEED, "the payer is made whole");
        assertEq(usdc.balanceOf(feeRecipient), 0, "no fee on a refund");
        assertEq(usdc.balanceOf(address(settlement)), 0, "the contract keeps nothing");
    }

    /// @dev refundAfter 之前，退款是 relayer 的決定：payer 自己來會被擋下。
    function test_refund_byThePayerBeforeTheWindowIsRejected() public {
        holdDefault();

        vm.prank(payer);
        vm.expectRevert(bytes("Settlement: the refund window is not open"));
        settlement.refund(REF);
    }

    /// @dev refundAfter 是 payer 的保底：時間一到，拿回自己的錢不需要任何人點頭。
    function test_refund_byThePayerAfterTheWindowNeedsNobody() public {
        holdDefault();

        vm.warp(refundAfter);
        vm.prank(payer);
        settlement.refund(REF);

        assertEq(usdc.balanceOf(payer), PAYER_SEED, "the payer refunds without the relayer");
    }

    /// @dev 退款的錢只會回到 payer 身上，但按按鈕的人也有限：不在名單上、又不是 payer 的
    ///      地址，過了 refundAfter 也碰不了這筆託管。
    function test_refund_byOutsiderIsRejected() public {
        holdDefault();

        vm.warp(refundAfter);
        vm.prank(outsider);
        vm.expectRevert(bytes("Settlement: caller cannot refund"));
        settlement.refund(REF);
    }

    /// @dev 退款不解鎖 ref：同一筆付款要重來，走的是新的 intent 與新的 ref，
    ///      跟狀態機「修正靠新 intent」同一條規則。
    function test_refund_doesNotReopenTheRef() public {
        holdDefault();
        vm.prank(relayer);
        settlement.refund(REF);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.settle(address(usdc), payer, merchant, AMOUNT, REF);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.hold(address(usdc), payer, merchant, AMOUNT, FEE, feeRecipient, REF, refundAfter);
    }

    // ====================================================================
    // 守恆
    // ====================================================================

    /// @dev 任意金額與手續費下守恆：merchant 加 feeRecipient 拿到的剛好是 payer 付出的，
    ///      合約一毛不留、token 總量不變。
    function testFuzz_holdAndReleaseConserveMoney(uint256 amount, uint256 fee) public {
        amount = bound(amount, 1, PAYER_SEED);
        fee = bound(fee, 0, amount - 1);

        vm.prank(relayer);
        settlement.hold(address(usdc), payer, merchant, amount, fee, feeRecipient, REF, refundAfter);
        vm.prank(relayer);
        settlement.release(REF);

        assertEq(usdc.balanceOf(payer), PAYER_SEED - amount);
        assertEq(usdc.balanceOf(merchant), amount - fee);
        assertEq(usdc.balanceOf(feeRecipient), fee);
        assertEq(usdc.balanceOf(address(settlement)), 0, "the contract keeps nothing");
        assertEq(usdc.totalSupply(), PAYER_SEED, "settlement mints and burns nothing");
    }
}
