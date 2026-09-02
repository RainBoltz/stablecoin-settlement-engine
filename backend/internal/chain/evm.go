package chain

import (
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// EVM 是 evm 協定的 adapter。四個答案沒有一個住在這裡：發號住在 txseq、不可逆規則住在
// finality、交易上限住在 bulk、替換規則住在 txfee，這裡只負責把它們對到同一個協定名下面。
//
// 它是四條鏈裡唯一一條每題都有完整答案的：自己發號（nonce）、能在同一個號上加價替換、
// 上限只有 gas 一條。之後接進來的鏈會一條比一條答得少，介面的形狀是照著答得少的那幾條設計的。
type EVM struct {
	seq *txseq.Counter
}

// NewEVM 建立 evm 的 adapter。sequencer 在這裡建、跟著 adapter 活一輩子：一條鏈的發號線在
// 一個 process 裡只能有一條（見 Adapter.Sequencer）。接真的鏈時要先對每個發送帳戶 Sync 一次，
// 跟 relayer.WithSequencer 的要求是同一件事。
func NewEVM() *EVM {
	return &EVM{seq: txseq.NewCounter()}
}

// Protocol 實作 Adapter。
func (e *EVM) Protocol() string { return "evm" }

// Sequencer 實作 Adapter：EVM 的 nonce 由發送方自己算，所以是一個真的發號器。
func (e *EVM) Sequencer() txseq.Sequencer { return e.seq }

// Finality 實作 Adapter：拿的是 finality.Defaults() 裡 evm 那一條，不自己抄一份。
func (e *EVM) Finality() finality.Policy { return finality.Defaults()["evm"] }

// BatchLimits 實作 Adapter：拿的是 bulk.Defaults() 裡 evm 那一條，出處跟著它走。
func (e *EVM) BatchLimits() bulk.Limits { return bulk.Defaults()["evm"] }

// ReplacementPolicy 實作 Replacer：EVM 上同一個帳戶、同一個 nonce 的交易最多一筆會進區塊，
// 所以同號加價重送是安全的。規則本身（起價、加價幅度、天花板、次數）住在 txfee。
func (e *EVM) ReplacementPolicy() txfee.Policy { return txfee.DefaultPolicy() }
