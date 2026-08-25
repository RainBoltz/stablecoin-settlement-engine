// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {IERC20} from "./interfaces/IERC20.sol";

/// @title Settlement
/// @notice 結算合約：搬錢的同時，把 PaymentRef 帶上鏈。
/// @dev 本合約為本系列從零設計，只取公開設計裡需要的那部分。
///
///      它要解的第一個問題不是搬錢——token 合約自己就會搬——而是 EVM 的 ERC-20 轉帳
///      沒有 memo 欄位：一筆裸的 transfer 上鏈之後，沒有任何地方寫著它是為了哪一筆付款，
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
///      一條搬錢路徑、兩個入口：
///      - settle()：pull 支付流。relayer 發起並代付 gas，動用 payer 事先給這份合約的
///        allowance。只有 relayer 名單上的地址能呼叫，不然任何人都能拿 payer 的
///        allowance 把錢搬給任意 merchant。
///      - pay()：push 支付流。payer 自己發起、自己付 gas，搬的是自己的錢，
///        所以不需要任何名單。它存在的理由是讓不走 relayer 的付款人也有地方放 ref。
///
///      刻意不做的事：
///      - 不碰錢過夜。transferFrom 直接從 payer 到 merchant，合約自己一毛都不持有，
///        所以它沒有提款、退款、pause 這些管理資產的函式。託管與手續費拆帳之後再談。
///      - 不量實收。fee-on-transfer 的 token 會讓 merchant 實際入帳少於請款，
///        本合約照樣放行、event 記的是請款金額：金額核對的判斷住在鏈下的 listener，
///        這裡不養第二份判斷。
///      - 不驗單筆授權。pull 支付流今天的信任模型是「payer 的 allowance 信任這份合約，
///        合約信任 relayer 名單」，名單上的 relayer 在額度內可以發起任何付款；
///        把授權收窄到「payer 對單筆付款簽名」的方案之後再談。
contract Settlement {
    /// @notice 部署者，唯一的權限是管理 relayer 名單。
    address public immutable owner;

    /// @notice pull 支付流的准入名單：只有名單上的地址能呼叫 settle()。
    mapping(address => bool) public isRelayer;

    /// @notice 已經結算過的 ref。一格 storage 換一個鏈上保證：同一筆付款不會走完第二次。
    mapping(bytes32 => bool) public paid;

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

    constructor() {
        owner = msg.sender;
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

    /// @dev 唯一的搬錢路徑，兩個入口共用，所以 replay 防護對兩條支付流一起生效。
    ///
    ///      順序是「先占 ref、再搬錢」，而且不能反過來：transferFrom 是外部呼叫，
    ///      token 的程式碼不歸我們管，一顆會重入的 token 可以在 ref 被標記之前拿同一個
    ///      ref 再進來一次，同一筆付款就走完了兩次。先標記就沒有這條路：重入進來撞到的
    ///      是「ref already paid」。失敗的情況不用擔心標記殘留——transferFrom 一 revert，
    ///      整筆交易連同標記一起回滾，ref 不會被一次失敗的嘗試燒掉。
    ///
    ///      transferFrom 的回傳值一定要檢查：EIP-20 允許 token 用回傳 false 代替 revert
    ///      （https://eips.ethereum.org/EIPS/eip-20），不檢查的話這種失敗會安靜地過，
    ///      event 照發、鏈下照對帳，一筆錢沒動的付款就成了幽靈支付。
    function _settle(address token, address payer, address merchant, uint256 amount, bytes32 ref) private {
        require(ref != bytes32(0), "Settlement: ref is zero");
        require(merchant != address(0), "Settlement: merchant is the zero address");
        require(amount > 0, "Settlement: amount is zero");
        require(!paid[ref], "Settlement: ref already paid");
        paid[ref] = true;

        bool ok = IERC20(token).transferFrom(payer, merchant, amount);
        require(ok, "Settlement: transferFrom returned false");

        emit Paid(ref, payer, merchant, token, amount, msg.sender);
    }
}
