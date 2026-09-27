package identity

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

func TestStartupDecryptsEveryActiveFactor(t *testing.T) {
	for _, state := range []string{factorEnabled, factorPending} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			principal, secret := f.admin.ID, f.adminSecret
			if state == factorPending {
				admin := f.adminLogin().Context
				p, code := f.register(admin, authz.Human, "pending-human")
				setup, err := f.svc.StartSetup(t.Context(), SetupRequest{Name: p.Name, Code: code, Channel: ChannelCLI})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.svc.SetPassword(t.Context(), setup.Context, "pending account password"); err != nil {
					t.Fatal(err)
				}
				enrollment, err := f.svc.EnrollFactor(t.Context(), setup.Context)
				if err != nil {
					t.Fatal(err)
				}
				principal, secret = p.ID, enrollment.raw
			}
			if err := f.svc.CheckStartup(t.Context()); err != nil {
				t.Fatalf("intact enabled and pending factors must open: %v", err)
			}
			factor, err := loadFactor(t.Context(), f.svc.main, principal, state)
			if err != nil {
				t.Fatal(err)
			}
			factor.Sealed[len(factor.Sealed)-1] ^= 0x80
			if _, err := f.svc.main.ExecContext(t.Context(), `UPDATE identity_totp_factors SET sealed_secret = ? WHERE factor_id = ?`, factor.Sealed, factor.ID); err != nil {
				t.Fatal(err)
			}
			err = f.svc.CheckStartup(t.Context())
			if !errors.Is(err, masterkey.ErrDecrypt) {
				t.Fatalf("damaged %s factor should prevent startup: %v", state, err)
			}
			for _, sensitive := range []string{totp.EncodeSecret(secret), base64.StdEncoding.EncodeToString(factor.Sealed), hex.EncodeToString(factor.Sealed)} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatal("startup diagnostics exposed secret material")
				}
			}
		})
	}
}

func TestStartupRejectsDifferentKeyMaterialUnderSameID(t *testing.T) {
	f := newFixture(t)
	other, err := masterkey.Generate(f.gen, f.clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := masterkey.Save(dir, other); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(masterkey.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	wire["key_id"] = f.key.ID()
	raw, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(masterkey.Path(dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	wrong, err := masterkey.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.newService(wrong).CheckStartup(t.Context()); !errors.Is(err, masterkey.ErrDecrypt) {
		t.Fatalf("matching key ID must not bypass real decryption: %v", err)
	}
}

func TestStartupIgnoresDisabledFactors(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.main.ExecContext(t.Context(), `UPDATE identity_totp_factors SET state = 'disabled', sealed_secret = ? WHERE principal_id = ?`, []byte("corrupt retired factor"), f.admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CheckStartup(t.Context()); err != nil {
		t.Fatalf("retired factors should not block startup: %v", err)
	}
}
