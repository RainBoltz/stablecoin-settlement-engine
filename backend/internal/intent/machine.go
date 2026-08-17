package intent

import (
	"errors"
	"fmt"
	"time"
)

// Request 是「我想把這筆 intent 推到 To」的請求。誰提的（By）、拿什麼證據（TxHash / Reason）、
// 什麼時候（At）都要寫明。At 由呼叫端傳進來而不是在裡面 time.Now()，狀態機才測得動。
type Request struct {
	To     State
	By     Actor
	TxHash string
	Reason string
	At     time.Time
}

// 拒絕的理由分開列，呼叫端才分得出「這是 bug」（ErrIllegalTransition、ErrForbiddenActor）
// 與「這是正常的競爭結果」（ErrTerminal、ErrVersionConflict）。前者要告警，後者要放手。
var (
	// ErrTerminal：intent 已在終態，任何轉移都拒絕。
	ErrTerminal = errors.New("intent: state is terminal")
	// ErrIllegalTransition：(from, to) 不在轉移表上。
	ErrIllegalTransition = errors.New("intent: transition not in table")
	// ErrForbiddenActor：這一列存在，但提出請求的角色沒資格走。
	ErrForbiddenActor = errors.New("intent: actor may not perform this transition")
	// ErrMissingEvidence：這一列要求 tx hash 或 reason，請求沒帶。
	ErrMissingEvidence = errors.New("intent: transition requires evidence")
	// ErrEvidenceMismatch：要宣告 settled 的交易雜湊，跟 confirming 時記下的不是同一筆。
	ErrEvidenceMismatch = errors.New("intent: tx hash does not match the one on record")
	// ErrExpired：簽名迴圈逾時，created 不能再變 authorized。
	ErrExpired = errors.New("intent: authorization window has passed")
	// ErrUnknownState：請求的 To 不是表上的狀態。
	ErrUnknownState = errors.New("intent: unknown state")
)

// Apply 嘗試把 it 推到 req.To。合法就就地修改 it（State、Version、TxHash、History）並回傳 applied=true；
// 不合法就一個欄位都不動，回傳錯誤。
//
// applied=false 且 err=nil 是第三種結果：it 已經在 req.To 了。這在分散式系統裡是常態不是錯誤：
// queue 會重送同一個 job、listener 會重掃同一個區塊，同一個事件送到兩次時第二次要能安靜地過。
// 呼叫端只要看 err；applied 只是讓它知道這次有沒有真的動到東西。
//
// 檢查順序是刻意排的，從最便宜、最常見的擋起：
//  1. 重放（已在目標狀態）：放行，不動。
//  2. 終態：拒絕。
//  3. (from, to) 不在表上：拒絕。
//  4. 角色不對：拒絕。
//  5. 證據不齊：拒絕。
//  6. 個別轉移的額外條件（逾時、雜湊要對得上）：拒絕。
func Apply(it *Intent, req Request) (applied bool, err error) {
	if !req.To.Valid() {
		return false, fmt.Errorf("%w: %q", ErrUnknownState, req.To)
	}
	if it.State == req.To {
		return false, nil
	}
	if it.State.Terminal() {
		return false, fmt.Errorf("%w: %s is %s", ErrTerminal, it.ID, it.State)
	}
	rule, ok := Lookup(it.State, req.To)
	if !ok {
		return false, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, it.State, req.To)
	}
	if !rule.Allows(req.By) {
		return false, fmt.Errorf("%w: %s -> %s by %s", ErrForbiddenActor, it.State, req.To, req.By)
	}
	if rule.NeedsTxHash && req.TxHash == "" {
		return false, fmt.Errorf("%w: %s -> %s needs tx hash", ErrMissingEvidence, it.State, req.To)
	}
	if rule.NeedsReason && req.Reason == "" {
		return false, fmt.Errorf("%w: %s -> %s needs reason", ErrMissingEvidence, it.State, req.To)
	}
	if err := guard(it, req); err != nil {
		return false, err
	}

	it.History = append(it.History, Transition{
		From: it.State, To: req.To, By: req.By, At: req.At, TxHash: req.TxHash, Reason: req.Reason,
	})
	it.State = req.To
	it.Version++
	it.UpdatedAt = req.At
	switch req.To {
	case StateConfirming:
		// 進區塊了：這一筆就是「目前認定在鏈上的交易」。
		it.TxHash = req.TxHash
	case StateSettling:
		// 只有 reorg 的回頭路會走到這裡（authorized -> settling 時 TxHash 本來就是空的）。
		// 交易被吐回來就不算在鏈上了，清掉，下一次 confirming 得重新拿出雜湊。
		it.TxHash = ""
	case StateSettled:
		it.TxHash = req.TxHash
	}
	return true, nil
}

// guard 是轉移表之外、個別轉移才有的條件。表管「誰可以走哪條路」，guard 管「走這條路還要滿足什麼」。
func guard(it *Intent, req Request) error {
	switch {
	case it.State == StateCreated && req.To == StateAuthorized:
		// 簽名迴圈有期限。過期的簽名就算驗得過也不收：付款人授權的是「那個時間點之前」的付款。
		if !it.ExpiresAt.IsZero() && req.At.After(it.ExpiresAt) {
			return fmt.Errorf("%w: expired at %s, request at %s", ErrExpired,
				it.ExpiresAt.UTC().Format(time.RFC3339), req.At.UTC().Format(time.RFC3339))
		}
	case req.To == StateSettled:
		// 宣告 settled 的雜湊必須就是 confirming 時記下的那一筆。不一樣代表 listener 看到的
		// 跟 relayer 送的不是同一筆交易，這種事不能靜靜地過，要停下來讓人看。
		if it.TxHash != "" && it.TxHash != req.TxHash {
			return fmt.Errorf("%w: on record %s, got %s", ErrEvidenceMismatch, it.TxHash, req.TxHash)
		}
	}
	return nil
}
