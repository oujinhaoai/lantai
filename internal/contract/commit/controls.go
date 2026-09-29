package commit

import (
	"context"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Controls is implemented by the authoritative ledger. Callers hold the shared
// security guard through their final acceptance. Raw Reader methods deliberately
// keep tombstones/history available to recovery; ordinary reads/writes must also
// pass these controls. No cached index or filesystem state substitutes for it.
type Controls interface {
	CheckVersionRead(context.Context, ids.ID, ids.ID) error
	CheckAssetWrite(context.Context, ids.ID) error
	CheckEvidenceAppend(context.Context, ids.ID, ids.ID) error
	CheckVersionSearch(context.Context, ids.ID, ids.ID, bool) error
}

// ReferenceRoots identifies live authoritative sources when provenance scans
// incoming uses for lifecycle decisions. Disabled and archived versions retain
// their references; trash/purge and a durable lifecycle write ban do not.
// Callers hold the security guard. Missing/corrupt control facts are errors.
type ReferenceRoots interface {
	RetainsReferences(context.Context, ids.ID, ids.ID) (bool, error)
}

// FileLocations is a trusted core read port, never a content authorization.
// Empty TrashID means live files. Pending moves must be reconciled; purged versions
// retain their ledger identity but no longer have readable manifest bytes.
type FileLocation struct {
	TrashID            ids.ID
	Purged             bool
	PendingOperationID ids.ID
}
type FileLocations interface {
	VersionFileLocation(context.Context, ids.ID, ids.ID) (FileLocation, error)
}
type RiskControls interface {
	CheckRiskRead(context.Context, ids.ID, ids.ID) error
}

// ReadFences prevent an old download capability from becoming usable again
// after lifecycle restoration. Authorization still runs on every request.
type ReadFences interface {
	VersionReadFence(context.Context, ids.ID, ids.ID) (int64, error)
}
