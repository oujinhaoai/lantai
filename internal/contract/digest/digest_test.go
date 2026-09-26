package digest

import (
	"io"
	"strings"
	"testing"
)

func TestFormats(t *testing.T) {
	// SHA-256("abc")
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	d := Of([]byte("abc"))
	if d != Digest(Prefix+abc) || d.Hex() != abc || !d.Valid() {
		t.Fatalf("Of(abc) = %s", d)
	}
	if _, err := Parse(string(d)); err != nil {
		t.Fatal(err)
	}
	if got, err := FromHex(abc); err != nil || got != d {
		t.Fatalf("FromHex = %s, %v", got, err)
	}
	for _, bad := range []string{
		abc,                              // 缺前缀
		"sha256:" + strings.ToUpper(abc), // 大写
		"sha512:" + abc,                  // 其他算法
		"sha256:" + abc[:63],             // 长度不足
		"sha256:" + abc + "0",            // 过长
		"sha256:" + abc[:63] + "g",       // 非十六进制
		" sha256:" + abc,                 // 空白
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if Digest("sha256:xyz").Hex() != "" {
		t.Error("Hex of invalid digest should be empty")
	}
	if ValidHex(strings.ToUpper(abc)) {
		t.Error("ValidHex must reject uppercase")
	}
}

func TestHasherStreams(t *testing.T) {
	h := NewHasher()
	if _, err := io.Copy(h, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if h.Size() != 3 || h.Digest() != Of([]byte("abc")) {
		t.Fatalf("hasher = %s (%d bytes)", h.Digest(), h.Size())
	}
}
