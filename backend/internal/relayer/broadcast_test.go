package relayer

import (
	"context"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// TestBroadcasts_LastAndCount：紀錄本回的是最後一筆加上一共幾筆，因為 Decide 只需要這兩件事。
func TestBroadcasts_LastAndCount(t *testing.T) {
	ctx := context.Background()
	b := NewMemoryBroadcasts()
	if _, tries, ok, err := b.Last(ctx, "pi_0001"); ok || tries != 0 || err != nil {
		t.Fatalf("empty log: tries=%d ok=%v err=%v", tries, ok, err)
	}
	for i, s := range []txseq.Sent{txseq.SentUnknown, txseq.SentYes} {
		if err := b.Record(ctx, Broadcast{IntentID: "pi_0001", Account: "0xabc", Nonce: uint64(7 + i),
			Ordered: true, Fee: txfee.NewFee(30, 2), Sent: s}); err != nil {
			t.Fatal(err)
		}
	}
	last, tries, ok, err := b.Last(ctx, "pi_0001")
	if err != nil || !ok || tries != 2 {
		t.Fatalf("tries=%d ok=%v err=%v", tries, ok, err)
	}
	if last.Sent != txseq.SentYes || last.Nonce != 8 {
		t.Fatalf("last = %s", last)
	}
	if _, _, ok, _ := b.Last(ctx, "pi_0002"); ok {
		t.Fatal("another intent should not see this one's rows")
	}
}

// TestBroadcasts_IsAppendOnly：一次送出去的交易是既成事實，就算後來被替換掉了它也發生過，
// 所以紀錄是多列，不是一列被改了幾次。
func TestBroadcasts_IsAppendOnly(t *testing.T) {
	ctx := context.Background()
	b := NewMemoryBroadcasts()
	_ = b.Record(ctx, Broadcast{IntentID: "pi_0001", Nonce: 7, Ordered: true, Sent: txseq.SentUnknown})
	_ = b.Record(ctx, Broadcast{IntentID: "pi_0001", Nonce: 7, Ordered: true, Fill: true, TxHash: "0xdead", Sent: txseq.SentYes})
	rows := b.All("pi_0001")
	if len(rows) != 2 || rows[0].TxHash != "" || rows[1].TxHash != "0xdead" {
		t.Fatalf("rows = %v", rows)
	}
	if got := rows[1].String(); got != "#7f  sent      0xdead   cap - tip -" {
		t.Fatalf("String() = %q", got)
	}
}
