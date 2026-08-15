// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {IERC20} from "../interfaces/IERC20.sol";

/// @title FeeOnTransferERC20Mock
/// @notice 每一次轉帳都抽稅的 token：收款方入帳金額永遠少於付款方轉出的金額。
/// @dev 行為依據：
///      - EIP-20：https://eips.ethereum.org/EIPS/eip-20
///      - 非標準 ERC-20 行為目錄：https://github.com/d-xo/weird-erc20
///
///      這隻與 USDTMock 的休眠稅是同一類問題，但費率在建構子就寫死、而且一開始就是開的，
///      方便把「入帳 ≠ 轉出」單獨拉出來測。
///
///      對結算系統的意義：
///      任何「先記帳 amount、再轉帳 amount」的流程都會產生差額。
///      正確作法是拿轉帳前後的餘額差當作實際入帳金額，而不是相信呼叫時傳進去的數字。
contract FeeOnTransferERC20Mock is IERC20 {
    string public name;
    string public symbol;
    uint8 public immutable decimals;

    /// @notice 費率，單位為 basis point（萬分之一）。
    uint256 public immutable feeBasisPoints;
    /// @notice 手續費收款位址。
    address public immutable feeRecipient;

    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    constructor(
        string memory name_,
        string memory symbol_,
        uint8 decimals_,
        uint256 feeBasisPoints_,
        address feeRecipient_
    ) {
        require(feeBasisPoints_ <= 10000, "FeeOnTransferERC20Mock: fee too high");
        require(feeRecipient_ != address(0), "FeeOnTransferERC20Mock: zero fee recipient");
        name = name_;
        symbol = symbol_;
        decimals = decimals_;
        feeBasisPoints = feeBasisPoints_;
        feeRecipient = feeRecipient_;
    }

    /// @inheritdoc IERC20
    function transfer(address _to, uint256 _value) external returns (bool) {
        _transfer(msg.sender, _to, _value);
        return true;
    }

    /// @inheritdoc IERC20
    function transferFrom(address _from, address _to, uint256 _value) external returns (bool) {
        uint256 allowed = allowance[_from][msg.sender];
        require(allowed >= _value, "FeeOnTransferERC20Mock: insufficient allowance");
        allowance[_from][msg.sender] = allowed - _value;
        _transfer(_from, _to, _value);
        return true;
    }

    /// @inheritdoc IERC20
    function approve(address _spender, uint256 _value) external returns (bool) {
        allowance[msg.sender][_spender] = _value;
        emit Approval(msg.sender, _spender, _value);
        return true;
    }

    /// @notice 測試與 devnet 專用的鑄幣 helper，刻意不做權限控制。
    function mint(address _to, uint256 _value) external {
        totalSupply += _value;
        balanceOf[_to] += _value;
        emit Transfer(address(0), _to, _value);
    }

    /// @dev 付款方扣 _value，收款方只拿到 _value - fee，差額進 feeRecipient。
    function _transfer(address from, address to, uint256 value) private {
        uint256 fromBalance = balanceOf[from];
        require(fromBalance >= value, "FeeOnTransferERC20Mock: insufficient balance");

        uint256 fee = (value * feeBasisPoints) / 10000;
        uint256 sendAmount = value - fee;

        unchecked {
            balanceOf[from] = fromBalance - value;
        }
        balanceOf[to] += sendAmount;

        if (fee > 0) {
            balanceOf[feeRecipient] += fee;
            emit Transfer(from, feeRecipient, fee);
        }
        emit Transfer(from, to, sendAmount);
    }
}
