package txseq

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Counter 是 EVM 的 nonce 與 TON 的 seqno 用的 Sequencer：每個帳戶一個遞增計數器，一次只發一個號。
//
// 「一次只發一個」是今天刻意選的保守值。真正的 relayer 會同時開好幾個號在飛（送出 7、8、9 不等 7 上鏈），
// 吞吐高很多，但只要中間有一個沒出門，後面那些就全部卡在 mempool 等它，而要把洞填掉需要另一套手段，之後會討論。
// 窗口是 1 的時候，退號永遠安全：手上那個號一定是最大的、也是唯一的，退回去就是把計數器往回撥一格。
//
// 提高吞吐的辦法因此不是把窗口開大，而是多開幾個發送帳戶：序列化的範圍是帳戶，不是整個 relayer。
//
// 計數器的真相在鏈上，不在這裡：程序重啟、或有人用同一個錢包在外面送了交易，都要靠 Sync 對回來
// （EVM 上就是 eth_getTransactionCount）。這裡記的只是「我發到哪了」。
type Counter struct {
	mu       sync.Mutex
	accounts map[string]*account
}

// account 是一個發送帳戶的狀態。sem 是容量 1 的 semaphore（Effective Go 的寫法，
// https://go.dev/doc/effective_go#channels），裡面有 token 代表現在沒有人占著這個帳戶。
type account struct {
	sem  chan struct{}
	next uint64
	// out 是發出去還沒收尾的那個號，nil 代表沒有。
	out *uint64
	// gap 是序列上那個沒交代的洞，nil 代表沒有。只有 SentUnknown 會製造它。
	gap *uint64
}

// NewCounter 建立一個 Counter。每個帳戶第一次被 Reserve 時從 0 開始；接真的鏈時要先 Sync 一次，
// 不然第一筆交易會拿到一個早就用過的號。
func NewCounter() *Counter {
	return &Counter{accounts: make(map[string]*account)}
}

// Reserve 實作 Sequencer。等到這個帳戶沒有人占著（或 ctx 結束），然後發下一個號。
//
// 帳戶上有洞的時候直接回 ErrGap 而不是等：洞不會自己消失，等下去只是把 lease 用完。
// 呼叫端拿到 ErrGap 應該原封不動把工作放回去，什麼都還沒寫，這時放手最便宜。
func (c *Counter) Reserve(ctx context.Context, account string) (Reservation, error) {
	a := c.account(account)
	select {
	case <-a.sem:
	case <-ctx.Done():
		return Reservation{}, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.gap != nil {
		a.sem <- struct{}{}
		return Reservation{}, fmt.Errorf("%w at %d", ErrGap, *a.gap)
	}
	v := a.next
	a.next++
	a.out = &v
	return Reservation{Account: account, Value: v, Ordered: true}, nil
}

// Resolve 實作 Sequencer。三種答案各自對計數器做一件事，見 Sent。
//
// 收尾之後帳戶就放開了，下一個等在 Reserve 的人可以進來；SentUnknown 的情況下它會拿到 ErrGap。
func (c *Counter) Resolve(_ context.Context, r Reservation, s Sent) error {
	if !r.Ordered {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.accounts[r.Account]
	if !ok || a.out == nil || *a.out != r.Value {
		return fmt.Errorf("%w: %s", ErrStale, r)
	}
	switch s {
	case SentNo:
		// 確定沒出門，把計數器撥回去。窗口是 1，所以這個號一定是最大的那個，撥回去不會蓋到別人。
		a.next = r.Value
	case SentUnknown:
		// 不知道，所以計數器不撥回去（那筆交易可能真的在鏈上），但這一格從此是個洞。
		v := r.Value
		a.gap = &v
	}
	a.out = nil
	a.sem <- struct{}{}
	return nil
}

// Sync 用鏈上回報的「下一個可用的號」對齊這個帳戶。EVM 上是 eth_getTransactionCount 的結果，
// 用已上鏈的那個（latest）不用 pending：pending 把還沒進區塊的也算進去，重啟後拿它對齊等於相信 mempool。
//
// 帳戶上還有號沒收尾的時候不給對齊（ErrBusy）：那一筆正在送，這時把計數器改掉，它收尾時就會對不上。
//
// 洞在這裡消失：鏈上的號已經走過那個洞，代表那筆下落不明的交易其實上鏈了。沒走過就留著，
// 序號不是我們想清掉就能清掉的東西。
func (c *Counter) Sync(_ context.Context, account string, next uint64) error {
	a := c.account(account)
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.out != nil {
		return fmt.Errorf("%w: %s #%d", ErrBusy, account, *a.out)
	}
	a.next = next
	if a.gap != nil && next > *a.gap {
		a.gap = nil
	}
	return nil
}

// Status 是一個帳戶目前的樣子，給 Example 與監控看。
type Status struct {
	Account  string
	Next     uint64
	InFlight bool
	Gap      uint64
	HasGap   bool
}

// String 用固定格式印一行，文章會直接貼這段輸出。
func (s Status) String() string {
	out := "-"
	if s.InFlight {
		out = "yes"
	}
	line := fmt.Sprintf("%s  next %-4d in-flight %-4s", shortAccount(s.Account), s.Next, out)
	if s.HasGap {
		return line + fmt.Sprintf(" gap %d", s.Gap)
	}
	return line + " gap -"
}

// Status 回報一個帳戶目前的樣子。沒看過的帳戶回一個全新的。
func (c *Counter) Status(account string) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{Account: account}
	a, ok := c.accounts[account]
	if !ok {
		return st
	}
	st.Next, st.InFlight = a.next, a.out != nil
	if a.gap != nil {
		st.Gap, st.HasGap = *a.gap, true
	}
	return st
}

// Accounts 回報看過的帳戶，排序過，Example 的輸出才固定。
func (c *Counter) Accounts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.accounts))
	for name := range c.accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// account 拿出（或建立）一個帳戶的狀態。新帳戶的 semaphore 一開始就有 token：沒有人占著。
func (c *Counter) account(name string) *account {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.accounts[name]
	if !ok {
		a = &account{sem: make(chan struct{}, 1)}
		a.sem <- struct{}{}
		c.accounts[name] = a
	}
	return a
}
