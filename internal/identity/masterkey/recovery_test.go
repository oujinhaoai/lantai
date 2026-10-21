package masterkey

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestRecoveryMaterialRoundTripIsExplicitAndBounded(t *testing.T) {
	k, e := Generate(&ids.Generator{Clock: clock.System{}, Rand: rand.Reader}, clock.System{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if e = k.WriteRecoveryMaterial(&out); e != nil {
		t.Fatal(e)
	}
	copy, e := ReadRecoveryMaterial(bytes.NewReader(out.Bytes()))
	if e != nil {
		t.Fatal(e)
	}
	sealed, e := k.Seal([]byte("synthetic secret"), []byte("factor"))
	if e != nil {
		t.Fatal(e)
	}
	opened, e := copy.Open(sealed, []byte("factor"))
	if e != nil || string(opened) != "synthetic secret" || copy.ID() != k.ID() {
		t.Fatal("key round trip failed")
	}
	//lint:ignore SA9005 Intentionally prove ordinary logging cannot serialize key material.
	raw, _ := json.Marshal(k)
	if string(raw) != "{}" {
		t.Fatal("ordinary serialization exported key")
	}
	if _, e = ReadRecoveryMaterial(bytes.NewReader(append(out.Bytes(), []byte("{}")...))); e == nil {
		t.Fatal("accepted trailing material")
	}
}
