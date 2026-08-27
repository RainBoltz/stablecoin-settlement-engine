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
///      搬錢的路徑有兩條。第一條當場結清，一筆交易裡錢從 payer 直達 merchant，四個入口：
///      - settle()：pull 支付流。relayer 發起並代付 gas，動用 payer 事先給這份合約的
///        allowance。只有 relayer 名單上的地址能呼叫，不然任何人都能拿 payer 的
///        allowance 把錢搬給任意 merchant。
///      - pay()：push 支付流。payer 自己發起、自己付 gas，搬的是自己的錢，
///        所以不需要任何名單。它存在的理由是讓不走 relayer 的付款人也有地方放 ref。
///      - settleWithPermit()：一樣是 pull 支付流，但 payer 給的不是這份合約的 allowance，
///        而是一份離線簽名。授權從「額度」變成「這一筆付款」，代價是金流路徑上多一份
///        外部合約（Permit2）。
///      - settleBatch()：settle() 的批次版。同一個 payer、同一顆 token，一筆交易對一批
///        merchant 各結一筆付款；每一項帶自己的 ref、發自己的 Paid，「批次」只存在於
///        交易這一層。
///      第二條先留後結，也就是託管：
///      - hold()：relayer 發起，把 payer 的錢先收進合約，記成一筆 Hold。
///      - release()：relayer 發起，替一筆託管拆帳：amount 減 fee 給 merchant，
///        fee 給 feeRecipient。
///      - refund()：把一筆託管全額退回 payer。relayer 隨時可以退；refundAfter 過了之後
///        payer 也可以自己退，不需要等任何人。
///      兩條路共用同一份 paid 標記：一個 ref 不管走哪條路，都只進得來一次。
///
///      刻意不做的事：
///      - 當場結清的四個入口不持有錢。transferFrom 直接從 payer 到 merchant，走完之後
///        合約帳上一毛不多。合約持有的只有託管中的錢，而且每一塊都對得回一筆 hold 記錄；
///        錢離開合約只有 release 與 refund 兩條路，去向都被那筆記錄釘死，所以它仍然
///        沒有 pause 或任意提款這類管理資產的函式。
///      - 不量實收，託管的入金除外。fee-on-transfer 的 token 會讓 merchant 實際入帳少於
///        請款，當場結清照樣放行、event 記的是請款金額：金額核對的判斷住在鏈下的
///        listener，這裡不養第二份判斷。hold() 是唯一的例外，理由寫在它的註解裡。
///      - 不算費率。fee 與 feeRecipient 都是 hold() 的參數：收多少、收到哪個地址，
///        判斷住在鏈下，合約只負責拆帳的執行。公開的前例一樣是 Request Network 的
///        ERC20FeeProxy：feeAmount 與 feeAddress 同樣是呼叫參數，合約只保證把
///        feeAmount 轉給 feeAddress。
///      - 不自動退款。refundAfter 只是把「payer 自己拿回錢」的門打開，沒有任何元件會
///        主動呼叫 refund()；一筆託管該結該退、什麼時候動手，都是鏈下的判斷。
///      - 批次不先集中再分發。公開前例 Disperse（https://github.com/banteg/disperse-research）
///        兩種形狀都做了：disperseToken 先把總額收進合約再逐筆 transfer 出去，
///        disperseTokenSimple 逐筆 transferFrom 直達收款人；這裡照後者，理由寫在
///        settleBatch() 的註解裡。
///      - 批次不逐筆容錯。中間任何一項失敗，整批回滾，沒有任何 ref 被占走；把壞的項目
///        剔掉是上鏈之前鏈下驗證的工作，之後再討論。
///      - 批次不設上限。真正的上限是區塊的 gas 上限，一批塞得下多少項，是鏈下組批時
///        照當下的鏈況判斷的事，寫死在合約裡只會變成第二份判斷。
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

    /// @notice 已經占用過的 ref。一筆 storage 寫入換一個鏈上保證：同一筆付款不會走完第二次。
    /// @dev 當場結清的入口在搬錢之前占用；託管在 hold() 收錢之前就占用，而且 refund 之後
    ///      也不釋放：退了款要重新收，走的是新的 intent 與新的 ref，跟狀態機
    ///      「修正靠新 intent」同一條規則。名字對託管來說收得早了一點，但這份 mapping 的
    ///      工作從第一天就只有一個：同一個 ref 只進來一次。
    mapping(bytes32 => bool) public paid;

    /// @notice 一筆託管中的付款：錢已經從 payer 收進合約，還沒拆帳給 merchant、也還沒退回。
    /// @dev 名字沿用 ledger 那邊的 hold entry：同一個概念（錢先卡位、還沒結清）在鏈上的
    ///      實體。這筆記錄只在錢真的入帳之後才寫下（見 hold()），所以「記錄存在」就等於
    ///      「錢在合約裡」，release() 與 refund() 不必再檢查餘額。
    ///      refundAfter 用 uint64 是為了跟 feeRecipient 擠同一個 storage slot，
    ///      放秒數的時間戳綽綽有餘。
    struct Hold {
        address token;
        address payer;
        address merchant;
        uint256 amount;
        uint256 fee;
        address feeRecipient;
        uint64 refundAfter;
    }

    /// @notice 託管中的付款，ref 對到它的 hold 記錄。release 或 refund 之後整筆刪掉，
    ///         所以「這筆託管結束了沒有」只有一種問法：記錄還在不在。
    mapping(bytes32 => Hold) public holds;

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

    /// @notice 一筆託管成立：錢已經從 payer 收進合約。
    /// @dev fee 在這一刻就講定、寫進 hold 記錄，release 只是照著拆，中途沒有人改得了條件。
    event Held(
        bytes32 indexed ref,
        address indexed payer,
        address indexed merchant,
        address token,
        uint256 amount,
        uint256 fee,
        uint64 refundAfter
    );

    /// @notice 一筆託管結清：amount 給了 merchant，fee 給了 feeRecipient。
    /// @dev 這裡的 amount 是 merchant 實際拿到的數字，加上 fee 剛好等於 Held 的 amount，
    ///      稽核不用自己再算一次。
    event Released(
        bytes32 indexed ref, address indexed merchant, address token, uint256 amount, uint256 fee, address feeRecipient
    );

    /// @notice 一筆託管退回：全額還給 payer，手續費不收。
    event Refunded(bytes32 indexed ref, address indexed payer, address token, uint256 amount);

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

    /// @notice 一筆撥款批次裡的一項：哪個 merchant、收多少錢、對到哪一個 ref。
    /// @dev 一項就是一筆完整的付款：自己的 ref、自己的 Paid、鏈下自己的 intent。批次只是
    ///      交易層的打包，付款的身分不因為同車而合併。token 與 payer 刻意不進這個 struct：
    ///      一個批次只有一個 payer、一顆 token，塞進每一項只是讓呼叫端多一種寫錯的方式；
    ///      要替第二個 payer 付錢，那是另一個批次。
    struct Payout {
        address merchant;
        uint256 amount;
        bytes32 ref;
    }

    /// @notice pull 支付流的批次版：同一個 payer、同一顆 token，一筆交易對一批 merchant
    ///         各結一筆付款。
    /// @dev 對鏈下來說「批次」不存在：每一項各走一次完整的 _settle（占自己的 ref、
    ///      搬自己的錢、發自己的 Paid），listener 與對帳引擎看到的是 N 筆各自帶 ref 的
    ///      付款，不需要知道它們來自同一筆交易。批次裡重複的 ref 也因此被同一道 replay 防護擋下。
    ///
    ///      錢逐筆從 payer 直達 merchant，不先集中進合約。Disperse 的 disperseToken 是
    ///      先把總額收進合約、再逐筆 transfer 出去，比逐筆 transferFrom 少付 N-1 次
    ///      allowance 的寫入；這裡照它的另一個形狀 disperseTokenSimple 逐筆直達，理由有二：
    ///      合約帳上不多一毛，「合約持有的每一塊錢都對得回一筆 hold 記錄」的不變量原樣
    ///      成立；fee-on-transfer 的短少也留在 payer 到 merchant 那一段，跟單筆 settle()
    ///      同一種語義，合約不必為了批次開始量實收。
    ///
    ///      中間任何一項失敗（token revert、回傳 false、allowance 用完），整批回滾：
    ///      沒有半筆錢動過、也沒有任何 ref 被占走，重試整批跟重試一筆一樣安全。
    ///      逐筆容錯（跳過壞的、只結好的）刻意不做：部分成功的批次是最難收拾的狀態，
    ///      哪幾筆成功要逐筆對 event 才知道；把壞的項目剔掉是上鏈之前鏈下驗證的工作。
    function settleBatch(address token, address payer, Payout[] calldata items) external {
        require(isRelayer[msg.sender], "Settlement: caller is not a relayer");
        require(items.length > 0, "Settlement: the batch is empty");
        for (uint256 i = 0; i < items.length; i++) {
            _settle(token, payer, items[i].merchant, items[i].amount, items[i].ref);
        }
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

    /// @notice 託管的入口：把 payer 的錢先收進合約，等 release() 拆帳或 refund() 退回。
    /// @dev 這是本合約唯一量實收的地方。當場結清不量，因為錢從 payer 直達 merchant，
    ///      短少多少是鏈下對帳的事；託管之後付出去的卻是合約自己的餘額，一顆
    ///      fee-on-transfer 的 token 要是在入金時抽走 1%，release 就會因為餘額不足而
    ///      revert，這筆託管從此結不了也退不成。與其收下一筆註定卡死的錢，不如在入口拒收。
    ///
    ///      hold 記錄刻意寫在搬錢之後：ref 在 _reserve 那一步就占住了，搬錢途中重入進來的
    ///      呼叫不管打哪個入口都會被擋，而記錄晚一步寫，「記錄存在」就永遠等於「錢已入帳」。
    ///
    ///      fee 與 feeRecipient 在這裡就講定、寫進記錄：release 只是照著拆，名單上的
    ///      其他 relayer 也改不了條件。refundAfter 是 payer 的保底：過了這個時間點，
    ///      退款不再需要任何人點頭（見 refund()）。
    function hold(
        address token,
        address payer,
        address merchant,
        uint256 amount,
        uint256 fee,
        address feeRecipient,
        bytes32 ref,
        uint64 refundAfter
    ) external {
        require(isRelayer[msg.sender], "Settlement: caller is not a relayer");
        _reserve(merchant, amount, ref);
        require(fee < amount, "Settlement: fee is not less than the amount");
        require(fee == 0 || feeRecipient != address(0), "Settlement: fee recipient is the zero address");
        require(refundAfter > block.timestamp, "Settlement: refundAfter is not in the future");

        uint256 balanceBefore = IERC20(token).balanceOf(address(this));
        IERC20(token).safeTransferFrom(payer, address(this), amount);
        require(IERC20(token).balanceOf(address(this)) - balanceBefore == amount, "Settlement: deposit arrived short");

        holds[ref] = Hold({
            token: token,
            payer: payer,
            merchant: merchant,
            amount: amount,
            fee: fee,
            feeRecipient: feeRecipient,
            refundAfter: refundAfter
        });

        emit Held(ref, payer, merchant, token, amount, fee, refundAfter);
    }

    /// @notice 結清一筆託管：amount 減 fee 給 merchant，fee 給 feeRecipient。
    /// @dev 先刪記錄、再搬錢，跟 _reserve 的「先占 ref、再搬錢」同一條規則：搬錢是外部
    ///      呼叫，一顆會重入的 token 殺回來時記錄已經不在，撞到的是「no hold for this
    ///      ref」。手續費在結清的這一刻才真的收到；退回去的託管一毛都不收（見 refund()）。
    function release(bytes32 ref) external {
        require(isRelayer[msg.sender], "Settlement: caller is not a relayer");
        Hold memory escrow = holds[ref];
        require(escrow.amount > 0, "Settlement: no hold for this ref");
        delete holds[ref];

        IERC20(escrow.token).safeTransfer(escrow.merchant, escrow.amount - escrow.fee);
        if (escrow.fee > 0) {
            IERC20(escrow.token).safeTransfer(escrow.feeRecipient, escrow.fee);
        }

        emit Released(ref, escrow.merchant, escrow.token, escrow.amount - escrow.fee, escrow.fee, escrow.feeRecipient);
    }

    /// @notice 退回一筆託管：全額還給 payer，包括本來要收的 fee。
    /// @dev relayer 隨時可以退；refundAfter 過了之後，payer 自己也可以退。後面這一條是
    ///      信任邊界的問題：錢躺在合約裡的期間，payer 不必相信「relayer 總有一天會來
    ///      處理」，時間一到自己就拿得回來。傳統支付的對應設計是授權的有效期限：
    ///      放到期限沒有請款的授權會失效，錢自動回到持卡人身上。
    ///
    ///      全額退是刻意的：手續費只在結清時收，一筆沒走完的付款不應該讓 payer 出錢。
    ///      退款之後 ref 仍然是占用的（見 paid 的註解），重新收款走新的 ref。
    function refund(bytes32 ref) external {
        Hold memory escrow = holds[ref];
        require(escrow.amount > 0, "Settlement: no hold for this ref");
        if (!isRelayer[msg.sender]) {
            require(msg.sender == escrow.payer, "Settlement: caller cannot refund");
            require(block.timestamp >= escrow.refundAfter, "Settlement: the refund window is not open");
        }
        delete holds[ref];

        IERC20(escrow.token).safeTransfer(escrow.payer, escrow.amount);

        emit Refunded(ref, escrow.payer, escrow.token, escrow.amount);
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

    /// @dev 每一個入口搬錢之前都要先經過這裡：檢查參數、確認 ref 沒用過，然後把它占下來。
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
