// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {IERC20} from "../interfaces/IERC20.sol";

/// @title NoRevertERC20Mock
/// @notice 餘額或授權不足時「回傳 false」而不是 revert 的 token。
/// @dev 行為依據：
///      - EIP-20：https://eips.ethereum.org/EIPS/eip-20
///      - 非標準 ERC-20 行為目錄：https://github.com/d-xo/weird-erc20
///
///      EIP-20 從頭到尾沒有要求失敗時必須 revert，只要求函式回傳 bool，
///      所以這個合約是 100% 合規的——不合規的是「忽略回傳值」的整合程式碼。
///
///      對結算系統來說這是最惡毒的一種陷阱：
///      交易上鏈成功、receipt.status == 1、gas 正常扣，但錢根本沒動。
///      如果 relayer 只看交易成功與否就把訂單標成 settled，帳就爛了（幽靈支付）。
///
///      除了「失敗不 revert」之外，其餘行為與 ERC20Mock 完全相同。
contract NoRevertERC20Mock is IERC20 {
    string public name;
    string public symbol;
    uint8 public immutable decimals;

    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    constructor(string memory name_, string memory symbol_, uint8 decimals_) {
        name = name_;
        symbol = symbol_;
        decimals = decimals_;
    }

    /// @inheritdoc IERC20
    function transfer(address _to, uint256 _value) external returns (bool) {
        // 餘額不足：安靜地回傳 false，不 revert、不發事件
        if (balanceOf[msg.sender] < _value) {
            return false;
        }
        _transfer(msg.sender, _to, _value);
        return true;
    }

    /// @inheritdoc IERC20
    function transferFrom(address _from, address _to, uint256 _value) external returns (bool) {
        // 授權或餘額不足一律回傳 false
        if (allowance[_from][msg.sender] < _value || balanceOf[_from] < _value) {
            return false;
        }
        allowance[_from][msg.sender] -= _value;
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

    function _transfer(address from, address to, uint256 value) private {
        unchecked {
            balanceOf[from] -= value;
        }
        balanceOf[to] += value;
        emit Transfer(from, to, value);
    }
}
