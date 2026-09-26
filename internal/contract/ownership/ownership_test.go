package ownership

import "testing"

func TestRegistryIsConsistent(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckTable(t *testing.T) {
	ok := []struct {
		db    Database
		table string
		owner string
	}{
		{Main, "identity_principals", "identity"},
		{Main, "extensions_registrations", "extensions"},
		{Runtime, "agent_execution_task_runs", "agent_execution"},
		{Runtime, "storage_upload_sessions", "storage"},
		{Ledger, "ledger_versions", "ledger"},
		{Ledger, "command_receipts", "commands"},
		{Index, "processed_events", "events"},
		{Events, "schema_migrations", "operations"},
		{Events, "sqlite_sequence", "sqlite"},
		{Index, "instance_binding", "operations"},
	}
	for _, c := range ok {
		owner, err := CheckTable(c.db, c.table)
		if err != nil || owner != c.owner {
			t.Errorf("%s.%s: %q %v", c.db, c.table, owner, err)
		}
	}
	for _, c := range []struct {
		db    Database
		table string
	}{
		{Main, "ledger_versions"},    // 台账表不能放主库
		{Ledger, "tasks_attempts"},   // 任务表不能放台账库
		{Events, "command_receipts"}, // 事件库没有业务回执
		{Index, "outbox"},            // 索引库不产生源事件
		{Runtime, "versions"},        // 缺模块前缀
		{"other", "identity_x"},      // 未知库
	} {
		if _, err := CheckTable(c.db, c.table); err == nil {
			t.Errorf("%s.%s accepted", c.db, c.table)
		}
	}
}

func TestEventOwner(t *testing.T) {
	if o, err := EventOwner("version.committed"); err != nil || o != "ledger" {
		t.Fatalf("%q %v", o, err)
	}
	if o, err := EventOwner("task_run.started"); err != nil || o != "agent_execution" {
		t.Fatalf("%q %v", o, err)
	}
	for _, bad := range []string{"committed", "unknown.thing"} {
		if _, err := EventOwner(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
