// Package httpapi adapts the M1 domain services to the public REST contract.
// It owns HTTP parsing and presentation, never SQL or domain state transitions.
package httpapi

import (
	"context"
	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type IdentityService interface {
	httpauth.Authenticator
	ExchangeToken(context.Context, string, identity.SessionRequest) (identity.IssuedSession, error)
	Login(context.Context, identity.LoginRequest) (identity.IssuedSession, error)
	WhoAmI(context.Context, authz.Context) (identity.Identity, error)
	EndSession(context.Context, authz.Context, ids.ID) error
	CreateChallenge(context.Context, authz.Context, identity.Command) (identity.Challenge, error)
	Rechallenge(context.Context, authz.Context, ids.ID, identity.Command) (identity.Challenge, error)
	VerifyChallenge(context.Context, authz.Context, ids.ID, string, string) (identity.Grant, error)
	Execute(context.Context, authz.Context, ids.ID, string, identity.Command) (identity.Result, error)
	GetPrincipal(context.Context, authz.Context, ids.ID) (identity.Principal, error)
	Members(context.Context, authz.Context, ids.ID) ([]identity.Member, int64, error)
	StartSetup(context.Context, identity.SetupRequest) (identity.IssuedSession, error)
	EnrollFactor(context.Context, authz.Context) (identity.Enrollment, error)
	SetPassword(context.Context, authz.Context, string) error
	ConfirmFactor(context.Context, authz.Context, string) ([]string, error)
}

type CatalogService interface {
	CreateProject(context.Context, catalog.ProjectRequest) (catalog.Project, error)
	ListProjects(context.Context, authz.Context, ids.ID, int) (catalog.ProjectPage, error)
	GetProject(context.Context, authz.Context, string) (catalog.Project, error)
	GetAsset(context.Context, authz.Context, ids.ID) (catalog.AssetInfo, error)
	GetVersion(context.Context, authz.Context, ids.ID, ids.ID) (catalog.VersionInfo, error)
	CommitVersion(context.Context, catalog.VersionRequest) (catalog.VersionResult, error)
	CancelVersion(context.Context, authz.Context, ids.ID) error
	PatchAsset(context.Context, authz.Context, string, ids.ID, int64, catalog.AssetPatch) (catalog.AssetDescription, error)
}

type StorageService interface {
	CreateUpload(context.Context, storage.CreateUploadRequest) (storage.Upload, error)
	GetUpload(context.Context, authz.Context, ids.ID) (storage.Upload, error)
	CompleteFile(context.Context, authz.Context, ids.ID, string) (storage.BlobGrant, error)
	CheckBlobs(context.Context, authz.Context, ids.ID, []storage.FileSpec) ([]storage.BlobStatus, error)
	IssueReadGrant(context.Context, storage.ReadRequest) (storage.ReadGrant, error)
}

type QueryService interface {
	Search(context.Context, query.SearchRequest) (query.Page, error)
}

// OperationReader must filter receipt ownership and every reference against
// current authorization. A raw ledger.Operation method does not satisfy it.
type OperationReader interface {
	Operation(context.Context, authz.Context, ids.ID) (*commands.View, error)
}

type Deps struct {
	Rights         *provenance.Service
	Ledger         *ledger.Service
	Reviews        *ledger.Reviews
	Lifecycle      *ledger.Lifecycle
	Discussions    *ledger.DiscussionObjects
	Evidence       *ledger.FileReviewSources
	Collaboration  *query.Collaboration
	ContextCatalog *catalog.Service
	Human          *identity.Service
	HumanTargets   identity.HumanTargets
	Tasks          *tasks.Service
	Flows          *workflow.Service
	Execution      *ax.Service
	Jobs           *jobs.Service
	Nodes          *node.Service
	Identity       IdentityService
	Catalog        CatalogService
	Storage        StorageService
	Query          QueryService
	Operations     OperationReader
}

type Config struct {
	InstanceID     ids.ID
	AllowedOrigins []string
	// MaxJSONBytes defaults to 8 MiB; it never applies to the transfer plane.
	MaxJSONBytes int64
	// APITimeout bounds JSON domain requests, not file streaming.
	APITimeout time.Duration
	// TransferIdleTimeout resets before each stream read/write; default two minutes.
	TransferIdleTimeout time.Duration
}
