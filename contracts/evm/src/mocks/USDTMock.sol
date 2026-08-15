// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

/// @title USDTMock
/// @notice 複製以太坊主網 Tether（USDT）公開驗證原始碼的行為集合，用來在本地重現整合陷阱。
/// @dev 本合約是依據以下公開資料所「描述的行為」從零撰寫，並非複製任何既有實作：
///      - TetherToken 主網驗證原始碼：
///        https://etherscan.io/address/0xdAC17F958D2ee523a2206206994597C13D831ec7#code
///      - EIP-20 標準（用來對照差異）：https://eips.ethereum.org/EIPS/eip-20
///      - 非標準 ERC-20 行為目錄：https://github.com/d-xo/weird-erc20
///
///      刻意保留的四個陷阱：
///
///      1. transfer / transferFrom / approve「沒有回傳值」。
///         原版寫於 Solidity 0.4.x，函式簽章不帶 returns (bool)，因此 returndata 是 0 bytes。
///         用標準 IERC20 介面呼叫時，Solidity 會在解碼 bool 之前先檢查 returndata 長度而 revert。
///         這也是本合約「刻意不繼承 IERC20」的原因——它的簽章根本對不上。
///
///      2. approve 歸零鎖：allowance 目前非 0 時，不能直接改成另一個非 0 值，必須先歸零。
///         這是原版為了緩解 approve race condition 而加的檢查，代價是所有「直接改額度」的流程都會 revert。
///
///      3. 休眠的轉帳稅：basisPointsRate / maximumFee 在主網上一直是 0，
///         但 owner 隨時能用 setParams 打開，讓收款方入帳短少。程式碼裡的死參數不等於永遠不會啟用。
///
///      4. 中心化管控：黑名單（凍結、甚至直接銷毀餘額）與全域 pause。
///
///      刻意不模擬的部分：原版還有 onlyPayloadSize 的短位址攻擊防護與 deprecated/upgradedAddress
///      的代理轉發。前者在 Solidity 0.8 的 calldata 解碼下已無實質作用，後者與今天的主題無關。
contract USDTMock {
    string public constant name = "Tether USD";
    string public constant symbol = "USDT";
    /// @notice 主網 USDT 是 6 位小數，不是 18；跨鏈金額換算最常見的錯誤來源之一。
    uint8 public constant decimals = 6;

    /// @notice 合約擁有者，同時也是轉帳稅的收款方。
    address public immutable owner;

    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    /// @notice 轉帳稅率，單位為 basis point（萬分之一）。
    uint256 public basisPointsRate;
    /// @notice 單筆轉帳稅上限，已乘上 10 ** decimals。
    uint256 public maximumFee;

    /// @notice 全域暫停開關，開啟時所有轉帳 revert。
    bool public paused;
    /// @notice 黑名單；被列入者無法轉出，且餘額可被 owner 直接銷毀。
    mapping(address => bool) public isBlackListed;

    event Transfer(address indexed _from, address indexed _to, uint256 _value);
    event Approval(address indexed _owner, address indexed _spender, uint256 _value);
    event Params(uint256 feeBasisPoints, uint256 maxFee);
    event AddedBlackList(address indexed _user);
    event RemovedBlackList(address indexed _user);
    event DestroyedBlackFunds(address indexed _blackListedUser, uint256 _balance);
    event Pause();
    event Unpause();

    modifier onlyOwner() {
        require(msg.sender == owner, "USDTMock: caller is not the owner");
        _;
    }

    modifier whenNotPaused() {
        require(!paused, "USDTMock: paused");
        _;
    }

    constructor() {
        owner = msg.sender;
    }

    // --------------------------------------------------------------------
    // 類 ERC-20 介面：注意這三個函式全部沒有回傳值
    // --------------------------------------------------------------------

    /// @notice 轉出 _value 給 _to。
    /// @dev 沒有 returns (bool)。這不是筆誤，是主網 USDT 的真實簽章。
    function transfer(address _to, uint256 _value) external whenNotPaused {
        // 忠於原版：只檢查付款方在不在黑名單，完全不管收款方是誰
        require(!isBlackListed[msg.sender], "USDTMock: sender is blacklisted");
        _move(msg.sender, _to, _value);
    }

    /// @notice 動用授權額度，從 _from 轉 _value 給 _to。
    /// @dev 同樣沒有 returns (bool)。
    function transferFrom(address _from, address _to, uint256 _value) external whenNotPaused {
        // 忠於原版：這裡檢查的是 _from，不是 msg.sender，也不是 _to
        require(!isBlackListed[_from], "USDTMock: from is blacklisted");

        uint256 allowed = allowance[_from][msg.sender];
        require(allowed >= _value, "USDTMock: insufficient allowance");
        // 忠於原版：無限授權（uint256 最大值）視為永久授權，不遞減
        if (allowed < type(uint256).max) {
            allowance[_from][msg.sender] = allowed - _value;
        }
        _move(_from, _to, _value);
    }

    /// @notice 設定 _spender 的授權額度。
    /// @dev 沒有 returns (bool)，而且帶著歸零鎖。
    function approve(address _spender, uint256 _value) external {
        // approve 歸零鎖：現有額度非 0 時，只能先改成 0，不能直接換成另一個非 0 值
        require(!((_value != 0) && (allowance[msg.sender][_spender] != 0)), "USDTMock: approve from non-zero allowance");
        allowance[msg.sender][_spender] = _value;
        emit Approval(msg.sender, _spender, _value);
    }

    // --------------------------------------------------------------------
    // 管理函式
    // --------------------------------------------------------------------

    /// @notice 調整轉帳稅率與單筆稅上限。
    /// @param newBasisPoints 稅率（萬分之一），必須小於 20，也就是上限 0.2%
    /// @param newMaxFee 單筆稅上限（以整顆代幣為單位），必須小於 50；存入時會乘上 10 ** decimals
    /// @dev 主網上這兩個值一直是 0，但這個函式一直都在。整合方不能假設它永遠不會被呼叫。
    function setParams(uint256 newBasisPoints, uint256 newMaxFee) external onlyOwner {
        require(newBasisPoints < 20, "USDTMock: basis points too high");
        require(newMaxFee < 50, "USDTMock: max fee too high");
        basisPointsRate = newBasisPoints;
        maximumFee = newMaxFee * (10 ** uint256(decimals));
        emit Params(basisPointsRate, maximumFee);
    }

    function addBlackList(address _evilUser) external onlyOwner {
        isBlackListed[_evilUser] = true;
        emit AddedBlackList(_evilUser);
    }

    function removeBlackList(address _clearedUser) external onlyOwner {
        isBlackListed[_clearedUser] = false;
        emit RemovedBlackList(_clearedUser);
    }

    /// @notice 直接銷毀黑名單位址的全部餘額，並同步減少 totalSupply。
    function destroyBlackFunds(address _blackListedUser) external onlyOwner {
        require(isBlackListed[_blackListedUser], "USDTMock: user is not blacklisted");
        uint256 dirtyFunds = balanceOf[_blackListedUser];
        balanceOf[_blackListedUser] = 0;
        totalSupply -= dirtyFunds;
        emit DestroyedBlackFunds(_blackListedUser, dirtyFunds);
    }

    function pause() external onlyOwner {
        paused = true;
        emit Pause();
    }

    function unpause() external onlyOwner {
        paused = false;
        emit Unpause();
    }

    /// @notice 測試與 devnet 專用的鑄幣 helper，刻意不做權限控制。
    /// @dev 原版對應的是 owner 專用的 issue()；這裡簡化成任意鑄幣，方便測試佈置初始餘額。
    function mint(address _to, uint256 _value) external {
        totalSupply += _value;
        balanceOf[_to] += _value;
        emit Transfer(address(0), _to, _value);
    }

    // --------------------------------------------------------------------
    // 內部搬帳
    // --------------------------------------------------------------------

    /// @dev 所有搬帳的唯一入口：先算稅、再動餘額、最後補事件。
    ///      transfer 與 transferFrom 的差別只在前置檢查，扣稅邏輯兩邊完全一致。
    function _move(address from, address to, uint256 value) private {
        uint256 fee = (value * basisPointsRate) / 10000;
        if (fee > maximumFee) {
            fee = maximumFee;
        }
        uint256 sendAmount = value - fee;

        uint256 fromBalance = balanceOf[from];
        require(fromBalance >= value, "USDTMock: insufficient balance");
        unchecked {
            balanceOf[from] = fromBalance - value;
        }
        balanceOf[to] += sendAmount;

        if (fee > 0) {
            balanceOf[owner] += fee;
            // 手續費會多發一筆 Transfer 事件。只用「to == 我的位址」過濾 log 的 indexer 不會漏，
            // 但只比對「金額 == 使用者輸入」的對帳邏輯會直接對不上。
            emit Transfer(from, owner, fee);
        }
        // 事件裡的金額是扣稅後的 sendAmount，不是呼叫端傳進來的 value
        emit Transfer(from, to, sendAmount);
    }
}
