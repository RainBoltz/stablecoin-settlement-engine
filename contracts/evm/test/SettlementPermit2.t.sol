// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Test} from "forge-std/Test.sol";

import {Settlement} from "../src/Settlement.sol";
import {ERC20Mock} from "../src/mocks/ERC20Mock.sol";
import {USDTMock} from "../src/mocks/USDTMock.sol";
import {Permit2Mock} from "./mocks/Permit2Mock.sol";

/// @notice payer 錢包那一側的工作：把一筆付款組成 Permit2 的 EIP-712 結構並簽下去。
/// @dev 這段刻意跟合約分開寫、也刻意不共用合約裡的常數：正式環境簽名的是使用者的錢包，
///      它只看得到公開規格。兩邊各自算一次、算出同一個 digest，才證明規格對得上。
///      fork 測試 import 同一份，換成主網真正的 domain separator 再簽一次。
abstract contract Permit2Signing is Test {
    bytes32 internal constant TOKEN_PERMISSIONS_TYPEHASH = keccak256("TokenPermissions(address token,uint256 amount)");
    bytes32 internal constant PAYMENT_WITNESS_TYPEHASH = keccak256("Payment(bytes32 ref,address merchant)");

    string internal constant WITNESS_TYPEHASH_STUB =
        "PermitWitnessTransferFrom(TokenPermissions permitted,address spender,uint256 nonce,uint256 deadline,";
    string internal constant PAYMENT_WITNESS_TYPE_STRING =
        "Payment witness)Payment(bytes32 ref,address merchant)TokenPermissions(address token,uint256 amount)";

    /// @notice 簽一筆付款。nonce 就是 ref，跟合約那一側的約定一致。
    /// @param spender 這份簽名授權誰去動這筆錢，也就是結算合約的位址。
    function signPayment(
        uint256 key,
        bytes32 domainSeparator,
        address spender,
        address token,
        uint256 amount,
        bytes32 ref,
        address merchant,
        uint256 deadline
    ) internal pure returns (bytes memory) {
        bytes32 structHash = keccak256(
            abi.encode(
                keccak256(abi.encodePacked(WITNESS_TYPEHASH_STUB, PAYMENT_WITNESS_TYPE_STRING)),
                keccak256(abi.encode(TOKEN_PERMISSIONS_TYPEHASH, token, amount)),
                spender,
                uint256(ref),
                deadline,
                keccak256(abi.encode(PAYMENT_WITNESS_TYPEHASH, ref, merchant))
            )
        );
        (uint8 v, bytes32 r, bytes32 s) =
            vm.sign(key, keccak256(abi.encodePacked("\x19\x01", domainSeparator, structHash)));
        return abi.encodePacked(r, s, v);
    }
}

/// @title SettlementPermit2Test
/// @notice 把簽名版的第三個入口釘死：一份簽名只買得到「payer 同意的那一筆付款」。
/// @dev 每一條「綁住了什麼」的測試都是同一種形狀：拿一份合法簽名、換掉其中一個欄位再送出去，
///      看它會不會被擋。擋不住的欄位就是 relayer 可以自己填的欄位。
contract SettlementPermit2Test is Permit2Signing {
    address internal owner;
    address internal relayer;
    address internal payer;
    uint256 internal payerKey;
    address internal merchant;
    address internal outsider;

    Permit2Mock internal permit2;
    Settlement internal settlement;
    ERC20Mock internal usdc;

    uint256 internal constant AMOUNT = 100e6;
    uint256 internal constant PAYER_SEED = 1_000_000e6;
    bytes32 internal constant REF = keccak256("day-18/ref-1");
    uint256 internal deadline;

    function setUp() public {
        owner = makeAddr("owner");
        relayer = makeAddr("relayer");
        merchant = makeAddr("merchant");
        outsider = makeAddr("outsider");
        (payer, payerKey) = makeAddrAndKey("payer");

        permit2 = new Permit2Mock();
        vm.prank(owner);
        settlement = new Settlement(address(permit2));
        vm.prank(owner);
        settlement.setRelayer(relayer, true);

        usdc = new ERC20Mock("USD Coin (mock)", "USDC", 6);
        usdc.mint(payer, PAYER_SEED);

        // payer 這輩子對這顆 token 只上鏈一次：把額度給 Permit2，之後每一筆付款都只簽名。
        vm.prank(payer);
        usdc.approve(address(permit2), type(uint256).max);

        deadline = block.timestamp + 15 minutes;
    }

    /// @dev 把一份合法簽名交給 relayer 送出去的 helper，後面每條測試都從它改一個欄位。
    function sign(address token, uint256 amount, bytes32 ref, address to) internal view returns (bytes memory) {
        return signPayment(payerKey, permit2.DOMAIN_SEPARATOR(), address(settlement), token, amount, ref, to, deadline);
    }

    // ====================================================================
    // 快樂路徑：payer 沒有對結算合約 approve 過任何東西
    // ====================================================================

    /// @dev 這條測試是今天的論點本體：settlement 的 allowance 是 0，錢照樣從 payer 到 merchant。
    ///      payer 這一筆付款一筆交易都沒發，gas 全部由 relayer 出。
    function test_settleWithPermit_movesMoneyWithoutAnAllowanceToTheContract() public {
        assertEq(usdc.allowance(payer, address(settlement)), 0, "the contract holds no allowance");

        bytes memory signature = sign(address(usdc), AMOUNT, REF, merchant);

        vm.expectEmit(true, true, true, true, address(settlement));
        emit Settlement.Paid(REF, payer, merchant, address(usdc), AMOUNT, relayer);

        vm.prank(relayer);
        settlement.settleWithPermit(address(usdc), payer, merchant, AMOUNT, REF, deadline, signature);

        assertEq(usdc.balanceOf(payer), PAYER_SEED - AMOUNT, "payer pays");
        assertEq(usdc.balanceOf(merchant), AMOUNT, "merchant receives");
        assertEq(usdc.balanceOf(address(settlement)), 0, "the contract never holds funds");
        assertEq(usdc.allowance(payer, address(settlement)), 0, "and still holds no allowance afterwards");
        assertTrue(settlement.paid(REF), "ref is marked paid");
    }

    /// @dev 有簽名也還是要在名單上：簽名證明的是 payer 同意這筆付款，不是「誰都可以送」。
    function test_settleWithPermit_rejectsCallerOutsideRelayerSet() public {
        bytes memory signature = sign(address(usdc), AMOUNT, REF, merchant);

        vm.prank(outsider);
        vm.expectRevert(bytes("Settlement: caller is not a relayer"));
        settlement.settleWithPermit(address(usdc), payer, merchant, AMOUNT, REF, deadline, signature);
    }

    // ====================================================================
    // 這份簽名到底綁住了什麼
    // ====================================================================

    /// @dev witness 存在的理由：Permit2 的 permit 本身沒有收款人欄位，收款地址是 spender
    ///      呼叫時自己填的。把 merchant 綁進 witness，relayer 就沒辦法把錢改送給自己。
    function test_settleWithPermit_signatureIsBoundToTheMerchant() public {
        bytes memory signature = sign(address(usdc), AMOUNT, REF, merchant);

        vm.prank(relayer);
        vm.expectRevert(Permit2Mock.InvalidSigner.selector);
        settlement.settleWithPermit(address(usdc), payer, outsider, AMOUNT, REF, deadline, signature);
    }

    /// @dev 金額也在簽名裡：多請一塊錢就是另一份 digest。
    function test_settleWithPermit_signatureIsBoundToTheAmount() public {
        bytes memory signature = sign(address(usdc), AMOUNT, REF, merchant);

        vm.prank(relayer);
        vm.expectRevert(Permit2Mock.InvalidSigner.selector);
        settlement.settleWithPermit(address(usdc), payer, merchant, AMOUNT + 1, REF, deadline, signature);
    }

    /// @dev spender 是 Permit2 用 msg.sender 補進 digest 的，簽名離開這份合約就不能用。
    ///      同一份 payer 簽名拿到另一份結算合約上送，算出來的 digest 不一樣。
    function test_settleWithPermit_signatureIsBoundToTheSpender() public {
        bytes memory signature = sign(address(usdc), AMOUNT, REF, merchant);

        vm.prank(owner);
        Settlement other = new Settlement(address(permit2));
        vm.prank(owner);
        other.setRelayer(relayer, true);

        vm.prank(relayer);
        vm.expectRevert(Permit2Mock.InvalidSigner.selector);
        other.settleWithPermit(address(usdc), payer, merchant, AMOUNT, REF, deadline, signature);
    }

    /// @dev deadline 是這條路徑獨有的新東西：簽名會過期，過期之後要回頭跟 payer 再要一份。
    function test_settleWithPermit_expiredSignatureIsRejected() public {
        bytes memory signature = sign(address(usdc), AMOUNT, REF, merchant);

        vm.warp(deadline + 1);
        vm.prank(relayer);
        vm.expectRevert(abi.encodeWithSelector(Permit2Mock.SignatureExpired.selector, deadline));
        settlement.settleWithPermit(address(usdc), payer, merchant, AMOUNT, REF, deadline, signature);
    }

    // ====================================================================
    // 兩份 replay 防護
    // ====================================================================

    /// @dev ref 直接當 Permit2 的 nonce 用：付款人的簽名不需要第二個識別碼，
    ///      而付款走完之後，那個 nonce 對應的 bit 在 Permit2 那邊也永久用掉了。
    function test_settleWithPermit_refIsTheNonce() public {
        uint256 word = uint256(REF) >> 8;
        uint256 bit = 1 << uint8(uint256(REF));
        assertEq(permit2.nonceBitmap(payer, word) & bit, 0, "the nonce starts unused");

        bytes memory signature = sign(address(usdc), AMOUNT, REF, merchant);
        vm.prank(relayer);
        settlement.settleWithPermit(address(usdc), payer, merchant, AMOUNT, REF, deadline, signature);

        assertEq(permit2.nonceBitmap(payer, word) & bit, bit, "and is used up afterwards");
    }

    /// @dev 同一份簽名重送：兩道防線都攔得住，先講話的是我們自己那道，因為它在呼叫 Permit2 之前。
    function test_settleWithPermit_sameRefPaysOnlyOnce() public {
        bytes memory signature = sign(address(usdc), AMOUNT, REF, merchant);

        vm.prank(relayer);
        settlement.settleWithPermit(address(usdc), payer, merchant, AMOUNT, REF, deadline, signature);

        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.settleWithPermit(address(usdc), payer, merchant, AMOUNT, REF, deadline, signature);

        assertEq(usdc.balanceOf(merchant), AMOUNT, "merchant is paid exactly once");
    }

    /// @dev replay 防護是整份合約共用的，跟走哪一個入口無關：push 進來的那一筆用掉 ref 之後，
    ///      簽名版拿同一個 ref 也走不完。
    function test_settleWithPermit_sharesTheReplayGuardWithTheOtherDoors() public {
        vm.prank(payer);
        usdc.approve(address(settlement), AMOUNT);
        vm.prank(payer);
        settlement.pay(address(usdc), merchant, AMOUNT, REF);

        bytes memory signature = sign(address(usdc), AMOUNT, REF, merchant);
        vm.prank(relayer);
        vm.expectRevert(bytes("Settlement: ref already paid"));
        settlement.settleWithPermit(address(usdc), payer, merchant, AMOUNT, REF, deadline, signature);
    }

    // ====================================================================
    // 代價與 token 的例外
    // ====================================================================

    /// @dev 一次性的 approve 沒有消失：payer 沒對 Permit2 授權過的 token，簽名再漂亮也搬不動。
    ///      失敗的那一筆不會燒掉 ref，跟另外兩個入口一樣。
    function test_settleWithPermit_missingApproveToPermit2DoesNotBurnTheRef() public {
        ERC20Mock dai = new ERC20Mock("Dai (mock)", "DAI", 18);
        dai.mint(payer, PAYER_SEED);

        bytes memory signature = sign(address(dai), AMOUNT, REF, merchant);
        vm.prank(relayer);
        vm.expectRevert(bytes("ERC20Mock: insufficient allowance"));
        settlement.settleWithPermit(address(dai), payer, merchant, AMOUNT, REF, deadline, signature);

        assertFalse(settlement.paid(REF), "a failed attempt does not burn the ref");
    }

    /// @dev 不回傳值的 token 走簽名版也一樣要過：搬錢那一步在 Permit2 裡面，
    ///      而它跟我們的封裝做同一件事——把三種回應收斂成「沒成功就 revert」。
    function test_settleWithPermit_usdtStyleTokenSettles() public {
        USDTMock usdt = new USDTMock();
        usdt.mint(payer, PAYER_SEED);
        vm.prank(payer);
        usdt.approve(address(permit2), type(uint256).max);

        bytes memory signature = sign(address(usdt), AMOUNT, REF, merchant);
        vm.prank(relayer);
        settlement.settleWithPermit(address(usdt), payer, merchant, AMOUNT, REF, deadline, signature);

        assertEq(usdt.balanceOf(merchant), AMOUNT, "merchant receives real tether-shaped tokens");
    }
}
