-- Host-side activation facts for one enablement generation: probe evidence is
-- bound to package, entry, config and host environment digests.
CREATE TABLE extensions_activations (
 enablement_id TEXT NOT NULL,
 generation INTEGER NOT NULL CHECK(generation>0),
 state TEXT NOT NULL CHECK(state IN ('pending_probe','ready','probe_failed')),
 environment_digest TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record)),
 PRIMARY KEY(enablement_id,generation)
) STRICT;
-- One row per dispatched one-shot invocation (the T06 job attempt ID). The
-- business result stays with T06; this is breaker and drain bookkeeping.
CREATE TABLE extensions_invocations (
 invocation_id TEXT PRIMARY KEY,
 enablement_id TEXT NOT NULL,
 generation INTEGER NOT NULL CHECK(generation>0),
 breaker_key TEXT NOT NULL,
 breaker_epoch INTEGER NOT NULL CHECK(breaker_epoch>0),
 half_open INTEGER NOT NULL CHECK(half_open IN (0,1)),
 state TEXT NOT NULL CHECK(state IN ('dispatched','completed','fault','cancelled','not_dispatched','unresolved')),
 admitted_at INTEGER NOT NULL,
 deadline INTEGER NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX extensions_invocations_key ON extensions_invocations(breaker_key,state);
CREATE TABLE extensions_breakers (
 breaker_key TEXT PRIMARY KEY,
 state TEXT NOT NULL CHECK(state IN ('closed','open')),
 epoch INTEGER NOT NULL CHECK(epoch>0),
 opened_at INTEGER NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
-- Faults are unique per invocation, so a timeout and a later exit count once.
CREATE TABLE extensions_breaker_faults (
 breaker_key TEXT NOT NULL,
 invocation_id TEXT NOT NULL,
 epoch INTEGER NOT NULL CHECK(epoch>0),
 at INTEGER NOT NULL,
 PRIMARY KEY(breaker_key,invocation_id)
) STRICT;
