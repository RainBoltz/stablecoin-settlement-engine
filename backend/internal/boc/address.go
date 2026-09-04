package boc

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Address 是 TON 的帳戶地址：一個 workchain 編號加上 256 bits 的帳戶雜湊。
// 零值代表 addr_none（message 表頭裡「沒有來源」寫的就是它），跟 paymentref 的零 ref 一樣，
// 名單上出現零值地址一律是還沒填。
type Address struct {
	Workchain int8
	Hash      [32]byte
}

// ErrBadAddress：讀不出地址。兩種寫法都認：raw 的 "0:" 加 64 個 hex，以及 48 個字元的
// user-friendly base64（EQ…／UQ… 那種，標準與 URL-safe 兩種字母表都收，CRC16 要對）。
var ErrBadAddress = errors.New("boc: not a TON address")

// IsZero 回報這是不是還沒填的零值。
func (a Address) IsZero() bool { return a == Address{} }

// String 印 raw 形式：workchain、冒號、64 個小寫 hex。這是四種寫法裡唯一沒有旗標可以選的一種，
// 拿來當 key 與印報告不會出現同一個地址兩種字串。
func (a Address) String() string {
	return strconv.Itoa(int(a.Workchain)) + ":" + hex.EncodeToString(a.Hash[:])
}

// Friendly 印 user-friendly 形式：36 bytes（旗標、workchain、雜湊、CRC16）的 URL-safe base64。
// bounceable 決定旗標 byte 是 0x11 還是 0x51：付給合約的 message 用 bounceable，錢才退得回來。
func (a Address) Friendly(bounceable bool) string {
	var p [36]byte
	p[0] = 0x51
	if bounceable {
		p[0] = 0x11
	}
	p[1] = byte(a.Workchain)
	copy(p[2:34], a.Hash[:])
	crc := crc16(p[:34])
	p[34], p[35] = byte(crc>>8), byte(crc)
	return base64.URLEncoding.EncodeToString(p[:])
}

// ParseAddress 讀 raw 或 user-friendly 形式的地址。
func ParseAddress(s string) (Address, error) {
	var a Address
	if wc, h, ok := strings.Cut(s, ":"); ok {
		n, err := strconv.ParseInt(wc, 10, 8)
		if err != nil || len(h) != 64 {
			return a, fmt.Errorf("%w: %q", ErrBadAddress, s)
		}
		if _, err := hex.Decode(a.Hash[:], []byte(h)); err != nil {
			return a, fmt.Errorf("%w: %q", ErrBadAddress, s)
		}
		a.Workchain = int8(n)
		return a, nil
	}
	if len(s) != 48 {
		return a, fmt.Errorf("%w: %q", ErrBadAddress, s)
	}
	p, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		if p, err = base64.StdEncoding.DecodeString(s); err != nil {
			return a, fmt.Errorf("%w: %q", ErrBadAddress, s)
		}
	}
	if len(p) != 36 || (p[0]&0x7f != 0x11 && p[0]&0x7f != 0x51) {
		return a, fmt.Errorf("%w: %q", ErrBadAddress, s)
	}
	if crc := crc16(p[:34]); p[34] != byte(crc>>8) || p[35] != byte(crc) {
		return a, fmt.Errorf("%w: %q has a bad checksum", ErrBadAddress, s)
	}
	a.Workchain = int8(p[1])
	copy(a.Hash[:], p[2:34])
	return a, nil
}

// crc16 是 user-friendly 地址尾端那兩個 bytes 用的 CRC-16/XMODEM（多項式 0x1021、初始值 0）。
func crc16(p []byte) uint16 {
	var crc uint16
	for _, b := range p {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
