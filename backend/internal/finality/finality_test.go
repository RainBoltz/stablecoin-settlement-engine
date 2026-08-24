package finality

import (
	"strings"
	"testing"
	"time"
)

// TestJudge_WaitsThenGivesUpOnAMissingTransaction：不在任何區塊裡的交易，年輕就等，超過 LostAfter 才判 lost。
// 防的是 RPC 落後一點就把一筆好好的付款退回 settling。
func TestJudge_WaitsThenGivesUpOnAMissingTransaction(t *testing.T) {
	p := Policy{Marker: "finalized", RequireMarker: true, LostAfter: 5 * time.Minute}
	if v := p.Judge(Observation{}, 4*time.Minute); v.Kind != KindPending {
		t.Fatalf("young and missing: got %s, want pending", v)
	}
	v := p.Judge(Observation{}, 5*time.Minute)
	if v.Kind != KindLost || !strings.Contains(v.Reason, "5m0s") {
		t.Fatalf("old and missing: got %s, want lost with the age", v)
	}
}

// TestJudge_NeverGivesUpWithoutLostAfter：LostAfter 為零代表永遠等。
func TestJudge_NeverGivesUpWithoutLostAfter(t *testing.T) {
	p := Policy{Marker: "finalized", RequireMarker: true}
	if v := p.Judge(Observation{}, 24*time.Hour); v.Kind != KindPending {
		t.Fatalf("got %s, want pending", v)
	}
}

// TestJudge_CountsConfirmationsIncludingItself：深度含自己，進區塊那一刻就是 1 個 confirmation。
func TestJudge_CountsConfirmationsIncludingItself(t *testing.T) {
	p := Policy{Confirmations: 2}
	in := Observation{Included: true, Height: 100, Head: 100, Succeeded: true}
	if v := p.Judge(in, 0); v.Kind != KindPending || !strings.Contains(v.Reason, "1 of 2") {
		t.Fatalf("head == height: got %s, want pending 1 of 2", v)
	}
	in.Head = 101
	if v := p.Judge(in, 0); v.Kind != KindFinal || !strings.Contains(v.Reason, "2 confirmations at 100") {
		t.Fatalf("head == height + 1: got %s, want final", v)
	}
}

// TestJudge_DepthIsZeroWhenTheNodeLagsBehind：Head 比 Height 小是節點落後，深度是 0 而不是一個繞回去的大數。
// 繞回去的話一個落後的節點會把任何交易都判成 final。
func TestJudge_DepthIsZeroWhenTheNodeLagsBehind(t *testing.T) {
	obs := Observation{Included: true, Height: 100, Head: 99, Succeeded: true}
	if d := obs.Depth(); d != 0 {
		t.Fatalf("depth = %d, want 0", d)
	}
	if v := (Policy{Confirmations: 1}).Judge(obs, 0); v.Kind != KindPending {
		t.Fatalf("got %s, want pending", v)
	}
}

// TestJudge_WaitsForTheChainsOwnMarker：深度再深，marker 沒到就不算。這是預設的那把尺。
func TestJudge_WaitsForTheChainsOwnMarker(t *testing.T) {
	p := Defaults()["evm"]
	obs := Observation{Included: true, Height: 100, Head: 164, Succeeded: true}
	if v := p.Judge(obs, 0); v.Kind != KindPending || !strings.Contains(v.Reason, "not yet finalized") {
		t.Fatalf("no marker: got %s, want pending", v)
	}
	obs.Final = true
	if v := p.Judge(obs, 0); v.Kind != KindFinal || v.Reason != "finalized at 100, 65 deep" {
		t.Fatalf("marker: got %s, want final", v)
	}
}

// TestJudge_BothKnobsMustPass：兩個旋鈕都開就兩個都要過，marker 到了但深度不夠一樣等。
func TestJudge_BothKnobsMustPass(t *testing.T) {
	p := Policy{Marker: "finalized", RequireMarker: true, Confirmations: 3}
	obs := Observation{Included: true, Height: 100, Head: 101, Final: true, Succeeded: true}
	if v := p.Judge(obs, 0); v.Kind != KindPending || !strings.Contains(v.Reason, "2 of 3") {
		t.Fatalf("got %s, want pending 2 of 3", v)
	}
	obs.Head = 102
	if v := p.Judge(obs, 0); v.Kind != KindFinal {
		t.Fatalf("got %s, want final", v)
	}
}

// TestJudge_FailureIsOnlyBelievedOnceFinal：revert 的交易在 finalized 之前跟成功的交易一樣可能被換掉，
// 所以失敗也要等到不可逆才判 failed。早一步送審，人看到的是一張可能翻盤的照片。
func TestJudge_FailureIsOnlyBelievedOnceFinal(t *testing.T) {
	p := Defaults()["evm"]
	obs := Observation{Included: true, Height: 100, Head: 110, Succeeded: false}
	if v := p.Judge(obs, 0); v.Kind != KindPending {
		t.Fatalf("reverted but not final: got %s, want pending", v)
	}
	obs.Final = true
	v := p.Judge(obs, 0)
	if v.Kind != KindFailed || !strings.Contains(v.Reason, "execution failed") {
		t.Fatalf("reverted and final: got %s, want failed", v)
	}
}

// TestDefaults_FourChainsAllWaitForTheirMarker：四條鏈的預設都等鏈自己的 marker，marker 各不相同，而且都有 LostAfter。
func TestDefaults_FourChainsAllWaitForTheirMarker(t *testing.T) {
	want := map[string]string{"evm": "finalized", "solana": "finalized", "ton": "masterchain", "sui": "checkpoint"}
	d := Defaults()
	if len(d) != len(want) {
		t.Fatalf("got %d chains, want %d", len(d), len(want))
	}
	for chain, marker := range want {
		p, ok := d[chain]
		switch {
		case !ok:
			t.Fatalf("%s: missing", chain)
		case !p.RequireMarker || p.Marker != marker:
			t.Fatalf("%s: got %s, want marker %s", chain, p, marker)
		case p.Confirmations != 0:
			t.Fatalf("%s: default should not count confirmations, got %d", chain, p.Confirmations)
		case p.LostAfter <= 0:
			t.Fatalf("%s: default needs a LostAfter", chain)
		}
	}
}

// TestPolicy_String：Example 與 log 印的那一行，四種組合各長什麼樣。
func TestPolicy_String(t *testing.T) {
	cases := []struct {
		p    Policy
		want string
	}{
		{Policy{Marker: "finalized", RequireMarker: true, LostAfter: 5 * time.Minute}, "final when finalized; lost after 5m0s"},
		{Policy{Confirmations: 2}, "final when 2 confirmations"},
		{Policy{Marker: "checkpoint", RequireMarker: true, Confirmations: 1}, "final when checkpoint and 1 confirmations"},
		{Policy{}, "final when included"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Fatalf("got %q, want %q", got, c.want)
		}
	}
}
