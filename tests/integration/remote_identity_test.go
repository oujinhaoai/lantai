package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// 仅初始本机管理员由 fixture 引导。项目授权、Agent 登记、凭据签发与会话兑换
// 全部走 CLI/REST/HumanGrant，证明没有依赖服务端数据库手工操作的使用缺口。
func TestRemoteIdentityAdministration(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	a := reopenApplication(t, e)
	s := remoteServers(t, a, true)
	work := t.TempDir()
	session := filepath.Join(work, "human.json")
	call := func(args ...string) []byte {
		t.Helper()
		var out, stderr bytes.Buffer
		args = append(args, "--server", "http://"+s.Addresses.API, "--allow-http", "--session-file", session)
		if code := cli.Run(t.Context(), args, &out, &stderr); code != 0 {
			t.Fatalf("%s: %d %s", args[0], code, stderr.String())
		}
		return out.Bytes()
	}
	credentials := filepath.Join(work, "credentials.json")
	writeJSON(t, credentials, map[string]string{"name": "ada", "password": adminPassword, "code": e.fresh()})
	call("login", "--credentials-file", credentials)
	if data := call("project", "list"); bytes.Contains(data, []byte(e.project.ProjectID)) {
		t.Fatal("admin implicitly owns project")
	}
	n := 0
	execute := func(action authz.Action, command any) ([]byte, string) {
		t.Helper()
		n++
		input := filepath.Join(work, fmt.Sprintf("command-%d.json", n))
		body := map[string]any{"action": action, "command": command}
		writeJSON(t, input, body)
		var ch identity.Challenge
		if err := json.Unmarshal(call("identity", "challenge", "--input", input), &ch); err != nil {
			t.Fatal(err)
		}
		if ch.Summary == "" || ch.OperationID == "" {
			t.Fatal("challenge omitted server summary")
		}
		writeJSON(t, credentials, map[string]string{"code": e.fresh()})
		var grant identity.Grant
		if err := json.Unmarshal(call("identity", "verify", "--id", string(ch.ChallengeID), "--credentials-file", credentials), &grant); err != nil {
			t.Fatal(err)
		}
		body["grant_id"] = grant.GrantID
		writeJSON(t, input, body)
		secretFile := filepath.Join(work, fmt.Sprintf("secret-%d.json", n))
		out := call("identity", "execute", "--input", input, "--idempotency-key", fmt.Sprintf("remote-admin-%d", n), "--secret-file", secretFile)
		if bytes.Contains(out, []byte(`"secret"`)) || bytes.Contains(out, []byte(`"token"`)) {
			t.Fatal("one-time secret escaped to stdout")
		}
		return out, secretFile
	}
	var members struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(call("identity", "members", "--project", string(e.project.ProjectID)), &members); err != nil {
		t.Fatal(err)
	}
	execute(identity.ActGrantProjectRole, &identity.SetProjectRole{ProjectID: e.project.ProjectID, ExpectedRevision: members.Revision, PrincipalID: e.admin.Context.PrincipalID, Role: identity.RoleOwner, Grant: true})
	if data := call("project", "list"); !bytes.Contains(data, []byte(e.project.ProjectID)) {
		t.Fatal("authorized project missing")
	}
	registered, _ := execute(identity.ActRegisterPrincipal, &identity.RegisterPrincipal{Kind: authz.Agent, Name: "cli@agent"})
	var registration struct {
		Result struct {
			ID ids.ID `json:"principal_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(registered, &registration); err != nil {
		t.Fatal(err)
	}
	if !registration.Result.ID.Valid() {
		t.Fatalf("registration missing identity: %s", registered)
	}
	var principal identity.Principal
	if err := json.Unmarshal(call("identity", "principal", "--id", string(registration.Result.ID)), &principal); err != nil {
		t.Fatal(err)
	}
	_, secretFile := execute(identity.ActIssueCredential, &identity.IssueCredential{PrincipalID: principal.ID, ExpectedRevision: principal.Revision, Scopes: []identity.Scope{identity.ScopeRead}})
	var issued struct {
		Secret string `json:"secret"`
	}
	if err := client.LoadState(secretFile, &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Secret == "" {
		t.Fatal("credential was not saved")
	}
	token := filepath.Join(work, "agent-token")
	if err := client.WritePrivate(token, []byte(issued.Secret)); err != nil {
		t.Fatal(err)
	}
	session = filepath.Join(work, "agent-session.json")
	call("session", "exchange", "--token-file", token)
	if data := call("whoami"); !bytes.Contains(data, []byte(principal.ID)) {
		t.Fatal("agent credential exchanged wrong identity")
	}
}
