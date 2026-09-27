package commands

import "testing"

func TestOperationStatsKnownAndUnknownStages(t *testing.T) {
	f := newFixture(t, "ledger", "ledger.db")
	stages := []Stage{StageReceiving, StagePrepared, StageInstalled, StageBlocked, StageQuarantined, StageCommitted, StageProjected, StageFailed, StageCancelled}
	for n, stage := range stages {
		_, err := f.db.ExecContext(t.Context(), `INSERT INTO operations(operation_id,owner_module,command_type,request_hash,recovery_epoch,stage,created_at,updated_at) VALUES(?, 'ledger','ledger.put_item','hash',1,?,0,0)`, n, stage)
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err := OperationStats(t.Context(), f.db)
	if err != nil || s.Pending != 5 || len(s.ByStage) != 9 {
		t.Fatalf("stats=%+v err=%v", s, err)
	}
	if _, err := f.db.ExecContext(t.Context(), `UPDATE operations SET stage='future_stage' WHERE operation_id='0'`); err != nil {
		t.Fatal(err)
	}
	if _, err := OperationStats(t.Context(), f.db); err == nil {
		t.Fatal("unknown stage silently accepted")
	}
}
