ALTER TABLE ledger_projects ADD COLUMN control_revision INTEGER NOT NULL DEFAULT 1 CHECK(control_revision>0);
