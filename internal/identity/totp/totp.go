// Package totp 实现 RFC 6238 的基于时间的一次性动态码（HMAC-SHA1、6 位、
// 30 秒时间步），与常见手机验证器兼容。
//
// 本包只做码值计算与时间步比较；“同一时间步只能成功一次”要求调用方把成功
// 的时间步与业务绑定在同一事务中持久化（last_accepted_counter），并把严格
// 更大的时间步作为唯一可接受的范围，见 RFC 6238 §5.2。
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

const (
	// Digits 是动态码位数。
	Digits = 6
	// Period 是时间步长度（秒）。
	Period = 30
	// Skew 是允许的前后时间步漂移。
	Skew = 1
	// SecretLen 是种子字节数（160 位，RFC 4226 推荐）。
	SecretLen = 20
	// Algorithm 是 otpauth 中的算法名称。
	Algorithm = "SHA1"
)

// NewSecret 随机生成种子。
func NewSecret(rnd io.Reader) ([]byte, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	s := make([]byte, SecretLen)
	if _, err := io.ReadFull(rnd, s); err != nil {
		return nil, fmt.Errorf("totp: read entropy: %w", err)
	}
	return s, nil
}

// Counter 返回 t 所在的时间步。
func Counter(t time.Time) int64 { return t.Unix() / Period }

// Code 返回种子在某个时间步的动态码（RFC 4226 动态截断）。
func Code(secret []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0F
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7FFFFFFF
	return fmt.Sprintf("%0*d", Digits, bin%1_000_000)
}

// Result 是一次校验的结论。
type Result int

const (
	// Invalid 表示动态码与允许窗口内任何时间步都不符。
	Invalid Result = iota
	// Accepted 表示动态码有效，且时间步大于上次成功的时间步。
	Accepted
	// Replayed 表示动态码有效，但时间步不大于上次成功的时间步（已被使用）。
	Replayed
)

// Verify 在 now 前后 Skew 个时间步内核对 code。lastAccepted 是该因子上次
// 成功的时间步（从未成功为 -1）。返回 Accepted 时 counter 是应持久化的新
// last_accepted_counter；多个时间步同时相符时取最大者，只会让窗口更紧。
func Verify(secret []byte, code string, now time.Time, lastAccepted int64) (Result, int64) {
	code = Normalize(code)
	if len(code) != Digits {
		return Invalid, 0
	}
	cur := Counter(now)
	var accepted, replayed bool
	var best int64 = -1
	for c := cur - Skew; c <= cur+Skew; c++ {
		if c < 0 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(Code(secret, c)), []byte(code)) != 1 {
			continue
		}
		if c > lastAccepted {
			accepted = true
			if c > best {
				best = c
			}
		} else {
			replayed = true
		}
	}
	switch {
	case accepted:
		return Accepted, best
	case replayed:
		return Replayed, 0
	default:
		return Invalid, 0
	}
}

// Normalize 去掉用户输入中的空格与连字符。
func Normalize(code string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, code)
}

// EncodeSecret 返回种子的 Base32（无填充）表示，供手工输入验证器。
func EncodeSecret(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

// URI 返回验证器登记用的 otpauth:// 地址。它包含种子，只能在登记当场展示
// 给本人，不得写入日志、事件或任何持久记录。
func URI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", Algorithm)
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(Period))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
