// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {CommonBase} from "forge-std/Base.sol";
import {ERC20Mock} from "../src/mocks/ERC20Mock.sol";
import {USDTMock} from "../src/mocks/USDTMock.sol";
import {NoRevertERC20Mock} from "../src/mocks/NoRevertERC20Mock.sol";
import {FeeOnTransferERC20Mock} from "../src/mocks/FeeOnTransferERC20Mock.sol";
import {DevnetAccounts} from "./DevnetAccounts.sol";

/// @title TokenZooBase
/// @notice 一條 devnet 需要的三件事，用純 Solidity 寫成，讓 script 與 test 都能繼承：
///         (1) 部署 Day 2 的 Token Zoo、(2) 注入「開工第一天」的世界狀態、(3) 讀寫 deployments/<chainId>.json。
/// @dev 這裡刻意沒有 run()。script 只加一層 CLI 入口（拿金鑰、廣播、寫檔），
///      test 直接繼承後在 setUp 裡呼叫 deploy() 與 seed()，對種子狀態下斷言。
///      同一份邏輯跑在 forge test 裡與跑在 Anvil 上，做出來的世界一模一樣，差別只在誰簽名。
abstract contract TokenZooBase is CommonBase {
    struct Zoo {
        address usdc;
        address usdt;
        address noRevert;
        address feeOnTransfer;
    }

    /// @dev 6 位小數的穩定幣：每個角色 100 萬。
    uint256 internal constant STABLE_SEED = 1_000_000e6;
    /// @dev 18 位小數的測試 token：每個角色 100 萬。
    uint256 internal constant TOKEN_SEED = 1_000_000e18;
    /// @dev USDT 發行商自己留一筆金庫，之後測「發行商動手」的情境（銷毀黑名單餘額、開稅）會用到。
    uint256 internal constant ISSUER_RESERVE = 100_000_000e6;
    /// @dev fee-on-transfer mock 的稅率：0.3%。
    uint256 internal constant FOT_FEE_BPS = 30;

    // ---------------------------------------------------------------- deploy

    /// @notice 部署四隻 mock。
    /// @param feeCollector fee-on-transfer mock 的抽稅收款人。
    /// @dev 為什麼要傳參數而不用 msg.sender：在 forge script 裡，msg.sender 是 forge 的預設 sender
    ///      (0x1804c8AB…)，不是正在廣播的那把金鑰。USDTMock 的 owner 沒這個問題，
    ///      因為它是在自己的建構子裡讀 msg.sender，那一格已經是廣播出去的交易了。
    function deploy(address feeCollector) public returns (Zoo memory zoo) {
        zoo.usdc = address(new ERC20Mock("USD Coin (mock)", "USDC", 6));
        zoo.usdt = address(new USDTMock());
        zoo.noRevert = address(new NoRevertERC20Mock("No Revert USD", "NRUSD", 18));
        zoo.feeOnTransfer =
            address(new FeeOnTransferERC20Mock("Fee On Transfer USD", "FOTUSD", 18, FOT_FEE_BPS, feeCollector));
    }

    // ------------------------------------------------------------------ seed

    /// @notice 注入結算系統「開工第一天」預期看到的世界：付款人與商家口袋有錢、發行商手上有金庫、
    ///         還有一個被列入黑名單的對手方。
    /// @dev 全部走正門（普通交易），所以同一份 seed 搬到 testnet 也能跑，不依賴 Anvil 的作弊 RPC。
    ///      呼叫者必須是 USDTMock 的 owner（addBlackList 只有 owner 能呼叫）。
    function seed(Zoo memory zoo, DevnetAccounts.Cast memory who) public {
        ERC20Mock(zoo.usdc).mint(who.payer, STABLE_SEED);
        ERC20Mock(zoo.usdc).mint(who.merchant, STABLE_SEED);

        USDTMock usdt = USDTMock(zoo.usdt);
        usdt.mint(usdt.owner(), ISSUER_RESERVE);
        usdt.mint(who.payer, STABLE_SEED);
        usdt.mint(who.merchant, STABLE_SEED);
        // 先給錢、再封鎖：手上有錢卻動不了，才是「永遠失敗」類真正的長相。
        usdt.mint(who.blacklisted, STABLE_SEED);
        usdt.addBlackList(who.blacklisted);

        NoRevertERC20Mock(zoo.noRevert).mint(who.payer, TOKEN_SEED);
        FeeOnTransferERC20Mock(zoo.feeOnTransfer).mint(who.payer, TOKEN_SEED);
    }

    // ------------------------------------------------------- deployments json

    /// @notice 交接檔的位置：deployments/<chainId>.json。之後 Solana / TON / SUI 各自用自己的識別碼。
    function deploymentsPath() public view returns (string memory) {
        return string.concat("deployments/", vm.toString(block.chainid), ".json");
    }

    function readZoo() public view returns (Zoo memory zoo) {
        string memory json = vm.readFile(deploymentsPath());
        zoo.usdc = vm.parseJsonAddress(json, ".tokens.USDC.address");
        zoo.usdt = vm.parseJsonAddress(json, ".tokens.USDT.address");
        zoo.noRevert = vm.parseJsonAddress(json, ".tokens.NRUSD.address");
        zoo.feeOnTransfer = vm.parseJsonAddress(json, ".tokens.FOTUSD.address");
    }

    /// @dev 這個檔案是合約層與鏈下層的交接點，之後 Go 後端與 cast 都從這裡拿地址。形狀：
    /// {
    ///   "chainId": 31337,
    ///   "deployer": "0x..",
    ///   "accounts": { "payer": "0x..", "merchant": "0x..", "relayer": "0x..", "blacklisted": "0x.." },
    ///   "tokens":   { "USDC": { "address": "0x..", "decimals": 6, "kind": "ERC20Mock" }, ... }
    /// }
    function toJson(Zoo memory zoo, address deployer) public returns (string memory) {
        DevnetAccounts.Cast memory who = DevnetAccounts.cast();

        string memory a = "accounts";
        vm.serializeAddress(a, "payer", who.payer);
        vm.serializeAddress(a, "merchant", who.merchant);
        vm.serializeAddress(a, "relayer", who.relayer);
        string memory accounts = vm.serializeAddress(a, "blacklisted", who.blacklisted);

        string memory t = "tokens";
        vm.serializeString(t, "USDC", _tokenJson("USDC", zoo.usdc, 6, "ERC20Mock"));
        vm.serializeString(t, "USDT", _tokenJson("USDT", zoo.usdt, 6, "USDTMock"));
        vm.serializeString(t, "NRUSD", _tokenJson("NRUSD", zoo.noRevert, 18, "NoRevertERC20Mock"));
        string memory tokens =
            vm.serializeString(t, "FOTUSD", _tokenJson("FOTUSD", zoo.feeOnTransfer, 18, "FeeOnTransferERC20Mock"));

        string memory root = "root";
        vm.serializeUint(root, "chainId", block.chainid);
        vm.serializeAddress(root, "deployer", deployer);
        vm.serializeString(root, "accounts", accounts);
        return vm.serializeString(root, "tokens", tokens);
    }

    function _tokenJson(string memory symbol, address token, uint8 decimals, string memory kind)
        private
        returns (string memory)
    {
        string memory obj = string.concat("token.", symbol);
        vm.serializeAddress(obj, "address", token);
        vm.serializeUint(obj, "decimals", decimals);
        return vm.serializeString(obj, "kind", kind);
    }
}
