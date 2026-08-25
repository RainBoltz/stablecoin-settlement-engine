// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {IERC20} from "./interfaces/IERC20.sol";
import {ISignatureTransfer} from "./interfaces/ISignatureTransfer.sol";
import {SafeTransfer} from "./libraries/SafeTransfer.sol";

/// @title Settlement
/// @notice 結算合約：搬錢的同時，把 PaymentRef 帶上鏈。
/// @dev 本合約為本系列從零設計，只取公開設計裡需要的那部分。
///
///      它要解的第一個問題是 EVM 的 ERC-20 轉帳沒有 memo 欄位（搬錢本身 token 合約
///      就會做）：一筆裸的 transfer 上鏈之後，沒有任何地方寫著它是為了哪一筆付款，
///      對帳引擎只能把它列成 unreferenced 等人工找回。錢繞經這份合約，ref 才有地方
///      以 event 的形式上鏈，listener 與對帳引擎才有東西可撈。
///      公開的前例是 Request Network 的 ERC20FeeProxy：把 paymentReference 當參數傳進
///      代理合約、寫進 event（https://github.com/RequestNetwork/requestNetwork/blob/master/
///      packages/advanced-logic/specs/payment-network-erc20-fee-proxy-contract-0.1.0.md）。
///
///      第二個問題是 replay：同一個 ref 只能結算一次。鏈下的去重（idempotency、nonce、
///      CAS）擋的是鏈下的重複，這裡的 paid mapping 是整條 pipeline 最後一道防線：
///      不管哪個元件出了什麼 bug、簽了幾筆交易，同一個 ref 在這份合約上只有一筆走得完。
///
///      一條搬錢路徑、三個入口：
///      - settle()：pull 支付流。relayer 發起並代付 gas，動用 payer 事先給這份合約的
///        allowance。只有 relayer 名單上的地址能呼叫，不然任何人都能拿 payer 的
///        allowance 把錢搬給任意 merchant。
///      - pay()：push 支付流。payer 自己發起、自己付 gas，搬的是自己的錢，
///        所以不需要任何名單。它存在的理由是讓不走 relayer 的付款人也有地方放 ref。
///      - settleWithPermit()：一樣是 pull 支付流，但 payer 給的不是這份合約的 allowance，
///        而是一份離線簽名。授權從「額度」變成「這一筆付款」，代價是金流路徑上多一份
///        別人的合約（Permit2）。
///
///      刻意不做的事：
///      - 不碰錢過夜。transferFrom 直接從 payer 到 merchant，合約自己一毛都不持有，
///        所以它沒有提款、退款、pause 這些管理資產的函式。託管與手續費拆帳之後再談。
///      - 不量實收。fee-on-transfer 的 token 會讓 merchant 實際入帳少於請款，
///        本合約照樣放行、event 記的是請款金額：金額核對的判斷住在鏈下的 listener，
///        這裡不養第二份判斷。
///      - allowance 那兩個入口不驗單筆授權。settle() 與 pay() 的信任模型是「payer 的
///        allowance 信任這份合約，合約信任 relayer 名單」，名單上的 relayer 在額度內
///        可以發起任何付款。要把授權收窄到單筆就走 settleWithPermit()：payer 簽的那份
///        EIP-712 綁著 ref 與 merchant，relayer 改一個欄位就驗不過。
contract Settlement {
    using SafeTransfer for IERC20;

    /// @notice 部署者，唯一的權限是管理 relayer 名單。
    address public immutable owner;

    /// @notice 驗簽並代為搬錢的 Permit2。
    /// @dev 位址由 constructor 傳入而不是寫死成常數：正式網路上 Permit2 每條鏈都在
    ///      0x000000000022D473030F116dDEE9F6B43aC78BA3，但本地 devnet 上沒有人部署過它，
    ///      離線測試也要能換成自己的替身。哪一條鏈填哪個位址是部署時的設定，不是合約的知識。
    ISignatureTransfer public immutable permit2;

    /// @notice pull 支付流的准入名單：只有名單上的地址能呼叫 settle()。
    mapping(address => bool) public isRelayer;

    /// @notice 已經結算過的 ref。一筆 storage 寫入換一個鏈上保證：同一筆付款不會走完第二次。
    mapping(bytes32 => bool) public paid;

    /// @dev payer 簽名時額外承諾的東西：這筆授權是為了哪一個 ref、錢要進哪一個 merchant。
    ///      Permit2 的 permit 本身只約束 token、金額、spender 與 deadline，收款人由 spender
    ///      呼叫時自己填；把這兩個欄位掛成 witness，簽名才等於「payer 同意的那一筆付款」。
    bytes32 private constant PAYMENT_WITNESS_TYPEHASH = keccak256("Payment(bytes32 ref,address merchant)");

    /// @dev EIP-712 要求引用到的型別按名稱排序後接在主型別後面，Permit2 用這個字串跟它自己的
    ///      前綴拼出完整 typehash。字串跟上面的 typehash 對不上時，算出來的 digest 就跟
    ///      payer 簽的那份不同，Permit2 會回 InvalidSigner。
    string private constant PAYMENT_WITNESS_TYPE_STRING =
        "Payment witness)Payment(bytes32 ref,address merchant)TokenPermissions(address token,uint256 amount)";

    /// @notice 錢在這筆交易裡動了。刻意不叫 Settled：鏈下 intent 的 `settled` 要等
    ///         finality 與金額核對，由 listener 宣告；這份合約能作證的只有「錢動過」。
    /// @dev ref 是 listener 與對帳引擎拿來對回 intent 的 key，所以放第一個 topic；
    ///      payer 與 merchant 也做成 topic，兩邊都能只訂閱跟自己有關的那份。
    ///      executor 是發起這筆結算的人：pull 支付流是 relayer，push 支付流是 payer 本人。
    event Paid(
        bytes32 indexed ref,
        address indexed payer,
        address indexed merchant,
        address token,
        uint256 amount,
        address executor
    );

    /// @notice relayer 名單的變更紀錄。
    event RelayerSet(address indexed relayer, bool allowed);

    modifier onlyOwner() {
        require(msg.sender == owner, "Settlement: caller is not the owner");
        _;
    }

    constructor(address permit2_) {
        owner = msg.sender;
        permit2 = ISignatureTransfer(permit2_);
    }

    /// @notice 把一個地址加入或移出 relayer 名單。
    function setRelayer(address relayer, bool allowed) external onlyOwner {
        isRelayer[relayer] = allowed;
        emit RelayerSet(relayer, allowed);
    }

    /// @notice push 支付流：payer 自己發起、自己付 gas。
    /// @dev 錢一樣要繞經合約（transferFrom 動用 msg.sender 給本合約的 allowance），
    ///      不然 ref 上不了鏈。這是 EVM 沒有 memo 欄位的代價：payer 得先多發一筆 approve。
    function pay(address token, address merchant, uint256 amount, bytes32 ref) external {
        _settle(token, msg.sender, merchant, amount, ref);
    }

    /// @notice pull 支付流：relayer 發起並代付 gas，動用 payer 事先給的 allowance。
    /// @dev 名單檢查擋的是「任何人都能替 payer 花 allowance」：transferFrom 的收款人
    ///      是呼叫端指定的，沒有這道檢查，任何人都能把 payer 授權的錢搬去任意地址。
    function settle(address token, address payer, address merchant, uint256 amount, bytes32 ref) external {
        require(isRelayer[msg.sender], "Settlement: caller is not a relayer");
        _settle(token, payer, merchant, amount, ref);
    }

    /// @notice pull 支付流的簽名版：payer 沒有給這份合約任何 allowance，只給了一份離線簽名。
    /// @dev payer 要做的一次性準備是對 Permit2 approve 一次該顆 token，之後每一筆付款只簽名、
    ///      不上鏈。這裡把 ref 直接當成 Permit2 的 nonce：Permit2 的 nonce 是無序 bitmap，
    ///      任何沒用過的 256 位元數字都可以，所以不必再發一個識別碼；同一筆 intent 重新請一次
    ///      簽名會拿到一模一樣的那份，重送也只會消耗同一個 bit。
    ///
    ///      deadline 是這條路徑獨有的新東西：簽名過期之後就要重新跟 payer 要一份，
    ///      「什麼時候要到期、過期了誰負責重簽」是鏈下的工作。
    function settleWithPermit(
        address token,
        address payer,
        address merchant,
        uint256 amount,
        bytes32 ref,
        uint256 deadline,
        bytes calldata signature
    ) external {
        require(isRelayer[msg.sender], "Settlement: caller is not a relayer");
        _reserve(merchant, amount, ref);

        permit2.permitWitnessTransferFrom(
            ISignatureTransfer.PermitTransferFrom({
                permitted: ISignatureTransfer.TokenPermissions({token: token, amount: amount}),
                nonce: uint256(ref),
                deadline: deadline
            }),
            ISignatureTransfer.SignatureTransferDetails({to: merchant, requestedAmount: amount}),
            payer,
            keccak256(abi.encode(PAYMENT_WITNESS_TYPEHASH, ref, merchant)),
            PAYMENT_WITNESS_TYPE_STRING,
            signature
        );

        emit Paid(ref, payer, merchant, token, amount, msg.sender);
    }

    /// @dev allowance 版的搬錢路徑，settle() 與 pay() 共用，所以 replay 防護對兩個入口
    ///      一起生效。
    ///
    ///      搬錢走 SafeTransfer 而非標準介面：token 對一筆轉帳可能 revert、可能回傳
    ///      false（EIP-20 允許，https://eips.ethereum.org/EIPS/eip-20）、成功時也可能
    ///      什麼都不回（主網 USDT）。SafeTransfer 把三種回應收斂成「沒成功就 revert」，
    ///      所以走到 emit 那一行時，錢一定動過了。
    function _settle(address token, address payer, address merchant, uint256 amount, bytes32 ref) private {
        _reserve(merchant, amount, ref);

        IERC20(token).safeTransferFrom(payer, merchant, amount);

        emit Paid(ref, payer, merchant, token, amount, msg.sender);
    }

    /// @dev 三個入口搬錢之前都要先經過這裡：檢查參數、確認 ref 沒用過，然後把它占下來。
    ///
    ///      順序是「先占 ref、再搬錢」，而且不能反過來：搬錢那一步是外部呼叫，不管走的是
    ///      token 自己還是 Permit2，程式碼都不歸我們管，一顆會重入的 token 可以在 ref 被
    ///      標記之前拿同一個 ref 再進來一次，同一筆付款就走完了兩次。先標記就沒有這條路：
    ///      重入進來撞到的是「ref already paid」。失敗的情況不用擔心標記殘留——搬錢一
    ///      revert，整筆交易連同標記一起回滾，ref 不會被一次失敗的嘗試燒掉。
    function _reserve(address merchant, uint256 amount, bytes32 ref) private {
        require(ref != bytes32(0), "Settlement: ref is zero");
        require(merchant != address(0), "Settlement: merchant is the zero address");
        require(amount > 0, "Settlement: amount is zero");
        require(!paid[ref], "Settlement: ref already paid");
        paid[ref] = true;
    }
}
