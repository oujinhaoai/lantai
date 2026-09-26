package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// 令牌前缀：长期凭据 ltk_、会话 lts_、CSRF ltc_。前缀让误贴到日志或聊天里
// 的令牌容易识别与吊销；只有会话令牌能调用业务接口。
const (
	CredentialPrefix = "ltk_"
	SessionPrefix    = "lts_"
	CSRFPrefix       = "ltc_"
)

const tokenBytes = 32

// newToken 返回带前缀的 32 字节随机令牌及其 SHA-256（小写十六进制）。库里只存
// 摘要：令牌本身有 256 位熵，不需要慢哈希。
func newToken(prefix string) (token, hash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", "", fmt.Errorf("identity: token entropy: %w", err)
	}
	token = prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, tokenHash(token), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// wellFormedToken 检查令牌形态，避免把任意长输入拿去查库。
func wellFormedToken(token, prefix string) bool {
	if !strings.HasPrefix(token, prefix) || len(token) != len(prefix)+43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(token[len(prefix):])
	return err == nil
}

// credentialDisplayPrefix 是保存与展示的明文前缀（前缀加 8 个字符），便于辨认。
func credentialDisplayPrefix(token string) string { return token[:len(CredentialPrefix)+8] }

// 恢复码与设置码：16 个 Crockford Base32 字符（80 位熵），显示为 4 组。
const (
	codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	codeLen      = 16
	codeSaltLen  = 16
)

func newCode() (string, error) {
	b := make([]byte, codeLen)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, v := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(codeAlphabet[v&31])
	}
	return sb.String(), nil
}

// normalizeCode 去掉分隔符并按 Crockford 规则纠正易混字符；格式不对返回空串。
func normalizeCode(in string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(in) {
		switch r {
		case '-', ' ':
			continue
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if !strings.ContainsRune(codeAlphabet, r) {
			return ""
		}
		sb.WriteRune(r)
	}
	if sb.Len() != codeLen {
		return ""
	}
	return sb.String()
}

func codeHash(salt []byte, normalized string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(normalized))
	return h.Sum(nil)
}

// sealCode 返回新码的盐与校验值。
func sealCode(code string) (salt, hash []byte, err error) {
	n := normalizeCode(code)
	if n == "" {
		return nil, nil, fmt.Errorf("identity: malformed generated code")
	}
	salt = make([]byte, codeSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, nil, err
	}
	return salt, codeHash(salt, n), nil
}

// codeMatches 以常数时间比较输入码与保存的校验值。
func codeMatches(input string, salt, hash []byte) bool {
	n := normalizeCode(input)
	if n == "" {
		return false
	}
	return subtle.ConstantTimeCompare(codeHash(salt, n), hash) == 1
}

// CSRFToken 返回浏览器会话的 CSRF 令牌：由主密钥派生的子密钥对会话 ID 做
// HMAC，不需要存储，随会话结束自然失效。它只在会话本人经同源读取时给出。
func (s *Service) CSRFToken(sessionID ids.ID) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte("lantai.csrf/v1\x00"))
	m.Write([]byte(sessionID))
	return CSRFPrefix + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) csrfValid(sessionID ids.ID, presented string) bool {
	want := s.CSRFToken(sessionID)
	return subtle.ConstantTimeCompare([]byte(want), []byte(presented)) == 1
}
