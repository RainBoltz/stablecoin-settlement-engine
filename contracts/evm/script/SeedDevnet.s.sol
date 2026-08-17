// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Script, console} from "forge-std/Script.sol";
import {TokenZooBase} from "./TokenZooBase.sol";
import {DevnetAccounts} from "./DevnetAccounts.sol";
import {ERC20Mock} from "../src/mocks/ERC20Mock.sol";
import {USDTMock} from "../src/mocks/USDTMock.sol";

/// @title SeedDevnet
/// @notice 對已經部署好的 Token Zoo 注入開工第一天的世界狀態。
/// @dev 用法：forge script script/SeedDevnet.s.sol --rpc-url anvil --broadcast
///      讀 DeployTokenZoo 寫下的 deployments/<chainId>.json。
///      設 USDT_TAX_BPS=10 可以在注入完成後順手把休眠中的 USDT 轉帳稅打開，用來重現「金額對不上」。
contract SeedDevnet is Script, TokenZooBase {
    function run() external {
        uint256 deployerKey = vm.envOr("DEPLOYER_KEY", DevnetAccounts.key(DevnetAccounts.DEPLOYER));
        Zoo memory zoo = readZoo();
        DevnetAccounts.Cast memory who = DevnetAccounts.cast();

        vm.startBroadcast(deployerKey);
        seed(zoo, who);
        vm.stopBroadcast();

        uint256 taxBps = vm.envOr("USDT_TAX_BPS", uint256(0));
        if (taxBps > 0) {
            vm.broadcast(deployerKey);
            USDTMock(zoo.usdt).setParams(taxBps, 20);
            console.log("USDT tax enabled: %s bps, capped at 20 USDT per transfer", taxBps);
        }

        _report(zoo, who);
    }

    function _report(Zoo memory zoo, DevnetAccounts.Cast memory who) private view {
        ERC20Mock usdc = ERC20Mock(zoo.usdc);
        USDTMock usdt = USDTMock(zoo.usdt);
        console.log("--- devnet seeded, chain %s ---", block.chainid);
        console.log(
            "payer       %s  USDC %s  USDT %s",
            who.payer,
            usdc.balanceOf(who.payer) / 1e6,
            usdt.balanceOf(who.payer) / 1e6
        );
        console.log(
            "merchant    %s  USDC %s  USDT %s",
            who.merchant,
            usdc.balanceOf(who.merchant) / 1e6,
            usdt.balanceOf(who.merchant) / 1e6
        );
        console.log("blacklisted %s  USDT %s (frozen)", who.blacklisted, usdt.balanceOf(who.blacklisted) / 1e6);
        console.log("relayer     %s  no tokens yet; pays gas for others later in the series", who.relayer);
    }
}
