// Package intent 是 Payment Intent 的狀態機：一筆付款從 API 進來到鏈上結束，
// 中間可以停在哪幾個狀態、每一步誰有權讓它往前走、往前走要拿出什麼證據。
//
// 這個 package 刻意只做「合不合法」的判斷，不碰資料庫、不碰鏈、不碰 queue。
// 之後的 API、relayer、chain listener 各自只負責提出「我想把它推到 X」的請求，
// 合不合法一律在這裡判；規則只寫在一張表上，改表就等於改系統行為，
// 所以測試會把整張表釘死（見 testdata/transitions.golden）。
package intent

// State 是 Payment Intent 生命週期中的一個停靠點。
//
// 為什麼是這八個：三個終態（settled / failed / canceled）之外，
// 每個非終態剛好對應「現在是誰在處理它」：created 與 authorized 是 API 的，
// settling 是 relayer 的，confirming 是 listener 的，needs_review 是人的。
// 狀態名同時也是責任歸屬。
type State string

const (
	// StateCreated：API 收下請求、intent 落地，待簽的 payload 已回給付款人，等簽名。
	StateCreated State = "created"
	// StateAuthorized：簽好的 payload 驗過了（Day 1 全景圖上寫的是 funded；
	// 改叫 authorized 是因為簽了名不代表錢在，錢在不在要到鏈上才知道）。
	StateAuthorized State = "authorized"
	// StateSettling：relayer 正在把交易送上鏈。重送、換手續費、換 nonce 都還在這一格，
	// 這些是「嘗試」，不是狀態；狀態只關心「有沒有一筆進區塊了」。
	StateSettling State = "settling"
	// StateConfirming：有一筆交易進區塊了，但還沒到 finality、金額也還沒核對。
	// 這一格存在的理由：進區塊不等於結束（reorg 會把它吐回來），
	// 但也不能當成沒送過（再送一次錢就多動一次）。
	StateConfirming State = "confirming"
	// StateSettled：終態。鏈上確認、金額對得上，錢動了一次。
	StateSettled State = "settled"
	// StateFailed：終態。確定沒動錢：交易明確失敗且不值得再試、或人判定失敗。
	StateFailed State = "failed"
	// StateCanceled：終態。還沒碰到鏈就收手：付款人沒簽、逾時、或商家取消。
	StateCanceled State = "canceled"
	// StateNeedsReview：系統不敢自己下判斷，等人。金額對不上、交易成功但餘額沒變、
	// 交易下落不明，都停在這裡。它不是終態，但從這裡出去只能是 settled 或 failed。
	StateNeedsReview State = "needs_review"
)

// States 依生命週期順序列出所有狀態：非終態在前、終態在後。順序固定，測試靠它判斷
// 一條轉移是「往前」還是「往回」，golden file 與文件也照這個順序。
func States() []State {
	return []State{
		StateCreated, StateAuthorized, StateSettling, StateConfirming, StateNeedsReview,
		StateSettled, StateFailed, StateCanceled,
	}
}

// Terminal 回報這個狀態是不是終態。終態沒有出口：修正靠新的 intent（沖正也是一筆新 intent），
// 不靠把舊 intent 改回去。
func (s State) Terminal() bool {
	switch s {
	case StateSettled, StateFailed, StateCanceled:
		return true
	}
	return false
}

// Valid 回報這是不是表上的八個狀態之一。
func (s State) Valid() bool {
	for _, known := range States() {
		if s == known {
			return true
		}
	}
	return false
}

// Actor 是有資格提出「把 intent 推到某個狀態」的角色。
//
// 這裡的角色是「系統裡的哪個元件」，不是「哪個使用者」：付款人與商家的動作都經由 API 進來，
// 所以只有 ActorAPI；同理 relayer 內部有幾個 worker 都算 ActorRelayer。
type Actor string

const (
	// ActorAPI：對外的 Payment API，代表 payer 與 merchant 做的事：建立、送簽名、取消。
	ActorAPI Actor = "api"
	// ActorRelayer：把交易送上鏈的元件。只有它能宣告「我開始送了」與「我放棄了」。
	ActorRelayer Actor = "relayer"
	// ActorListener：盯著鏈看的元件（chain listener 與之後的對帳引擎都算）。
	// 只有它能宣告「錢真的動了」，因為它是唯一從鏈上讀事實的人。
	ActorListener Actor = "listener"
	// ActorOperator：人。只在 needs_review 出手，而且只能收尾，不能重送。
	ActorOperator Actor = "operator"
	// ActorSystem：時鐘。逾時沒簽名的 intent 由它取消。
	ActorSystem Actor = "system"
)

// Actors 依固定順序列出所有角色。
func Actors() []Actor {
	return []Actor{ActorAPI, ActorRelayer, ActorListener, ActorOperator, ActorSystem}
}

// Rule 是轉移表上的一列：從哪裡、到哪裡、誰可以、要帶什麼證據。
type Rule struct {
	From State
	To   State
	// By 列出有權執行這一列的角色。不在名單上的角色一律拒絕，沒有「管理員全能」這種例外。
	By []Actor
	// NeedsTxHash：進入「有一筆交易在鏈上」的狀態時必須帶交易雜湊，空字串不收。
	NeedsTxHash bool
	// NeedsReason：所有「不是往前走的正常路」都要留一句為什麼，之後人與對帳引擎要看。
	NeedsReason bool
}

// Rules 是整張轉移表。不在表上的 (from, to) 一律非法，沒有預設放行。
//
// 表的形狀就是設計決定：
//   - 每個非終態的出口都只屬於一個擁有者（API / relayer / listener / operator）。
//   - 唯一的回頭路是 confirming → settling：reorg 會把已進區塊的交易吐回來，
//     relayer 得重新處理。這條路歸 listener，因為只有它看得到 reorg。
//   - needs_review 沒有回 settling 的路。交易下落不明時再送一次就可能付兩次；
//     人只能判定「已付」或「未付」，想再付就開一筆新的 intent。
//   - 終態沒有任何出口。
func Rules() []Rule {
	return []Rule{
		// API 的地盤：簽名迴圈
		{From: StateCreated, To: StateAuthorized, By: []Actor{ActorAPI}},
		{From: StateCreated, To: StateCanceled, By: []Actor{ActorAPI, ActorOperator, ActorSystem}, NeedsReason: true},
		{From: StateAuthorized, To: StateSettling, By: []Actor{ActorRelayer}},
		{From: StateAuthorized, To: StateCanceled, By: []Actor{ActorAPI, ActorOperator}, NeedsReason: true},

		// relayer 的地盤：把交易送進區塊
		{From: StateSettling, To: StateConfirming, By: []Actor{ActorRelayer, ActorListener}, NeedsTxHash: true},
		{From: StateSettling, To: StateFailed, By: []Actor{ActorRelayer}, NeedsReason: true},
		{From: StateSettling, To: StateNeedsReview, By: []Actor{ActorRelayer}, NeedsReason: true},

		// listener 的地盤：finality 與金額核對
		{From: StateConfirming, To: StateSettled, By: []Actor{ActorListener}, NeedsTxHash: true},
		{From: StateConfirming, To: StateSettling, By: []Actor{ActorListener}, NeedsReason: true},
		{From: StateConfirming, To: StateNeedsReview, By: []Actor{ActorListener}, NeedsReason: true},

		// 人的地盤：只能收尾
		{From: StateNeedsReview, To: StateSettled, By: []Actor{ActorOperator}, NeedsTxHash: true, NeedsReason: true},
		{From: StateNeedsReview, To: StateFailed, By: []Actor{ActorOperator}, NeedsReason: true},
	}
}

// Lookup 找出 (from, to) 這一列；不在表上回傳 ok=false。
func Lookup(from, to State) (Rule, bool) {
	for _, r := range Rules() {
		if r.From == from && r.To == to {
			return r, true
		}
	}
	return Rule{}, false
}

// Allows 回報 by 有沒有資格走這一列。
func (r Rule) Allows(by Actor) bool {
	for _, a := range r.By {
		if a == by {
			return true
		}
	}
	return false
}
