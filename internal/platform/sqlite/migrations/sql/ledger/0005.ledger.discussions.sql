CREATE TABLE ledger_messages (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 message_id TEXT NOT NULL UNIQUE,
 project_id TEXT NOT NULL,
 object_kind TEXT NOT NULL,
 object_id TEXT NOT NULL,
 operation_id TEXT NOT NULL UNIQUE,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX ledger_messages_object ON ledger_messages(project_id,object_kind,object_id,sequence);
