// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {IERC20} from "../interfaces/IERC20.sol";

/// @title ERC20Mock
/// @notice Token Zoo 的對照組：一個完全符合 EIP-20 的實作。
/// @dev 行為依據：https://eips.ethereum.org/EIPS/eip-20
///
///      這裡沒有任何驚喜，就是規格書上的樣子：
///      - transfer / transferFrom / approve 都回傳 bool
///      - 餘額或 allowance 不足時 revert（EIP-20 建議、且是目前的主流作法）
///      - 每一次成功的搬帳與授權都發出對應事件
///
///      動物園裡其他三隻的「怪異之處」，全部是拿這個合約當基準線來對照的。
contract ERC20Mock is IERC20 {
    string public name;
    string public symbol;
    uint8 public immutable decimals;

    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    /// @param name_ 代幣名稱
    /// @param symbol_ 代幣符號
    /// @param decimals_ 小數位數；刻意由建構子指定，方便在測試裡重現 6 / 8 / 18 位的差異
    constructor(string memory name_, string memory symbol_, uint8 decimals_) {
        name = name_;
        symbol = symbol_;
        decimals = decimals_;
    }

    /// @inheritdoc IERC20
    function transfer(address _to, uint256 _value) external returns (bool) {
        _transfer(msg.sender, _to, _value);
        return true;
    }

    /// @inheritdoc IERC20
    function transferFrom(address _from, address _to, uint256 _value) external returns (bool) {
        uint256 allowed = allowance[_from][msg.sender];
        require(allowed >= _value, "ERC20Mock: insufficient allowance");
        allowance[_from][msg.sender] = allowed - _value;
        _transfer(_from, _to, _value);
        return true;
    }

    /// @inheritdoc IERC20
    function approve(address _spender, uint256 _value) external returns (bool) {
        // 標準行為：不管原本額度是多少都直接覆寫，沒有任何前置條件
        allowance[msg.sender][_spender] = _value;
        emit Approval(msg.sender, _spender, _value);
        return true;
    }

    /// @notice 測試與 devnet 專用的鑄幣 helper，刻意不做權限控制。
    /// @dev 正式合約絕對不會有這種函式；這裡是為了讓測試能任意佈置初始餘額。
    function mint(address _to, uint256 _value) external {
        totalSupply += _value;
        balanceOf[_to] += _value;
        emit Transfer(address(0), _to, _value);
    }

    function _transfer(address from, address to, uint256 value) private {
        uint256 fromBalance = balanceOf[from];
        require(fromBalance >= value, "ERC20Mock: insufficient balance");
        unchecked {
            balanceOf[from] = fromBalance - value;
        }
        balanceOf[to] += value;
        emit Transfer(from, to, value);
    }
}
