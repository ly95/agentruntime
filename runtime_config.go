package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ly95/agentruntime/skills"
)

const (
	defaultMaxIterations          = 8
	defaultMaxCallsPerTurn        = 32
	defaultSessionLeaseTTL        = 30 * time.Second
	defaultSessionLeaseRenewal    = 10 * time.Second
	defaultDetachedCleanupTimeout = 5 * time.Second
)

type Input struct {
	// RunID is an optional trusted host-assigned identifier. Durable hosts that
	// enqueue work before execution use it to keep the HTTP, queue, store, and
	// event identities aligned. Supplying it requires a RunStore, which is the
	// durable authority that rejects reuse except for the exact waiting run being
	// resumed. It is intentionally excluded from JSON input so callers cannot
	// smuggle an arbitrary run identity through an API payload.
	RunID     string `json:"-"`
	User      string `json:"user"`
	SessionID string `json:"session_id,omitempty"`
	// IdempotencyKey identifies one logical user request. Callers must reuse it
	// when retrying a request that may execute write operations.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// IdempotencyScope isolates stateless write keys by trusted tenant or
	// principal. It is required for write operations without a SessionID.
	IdempotencyScope string                 `json:"-"`
	Attachments      []ModelInputAttachment `json:"attachments,omitempty"`
	// Metadata is normalized into the native exact-JSON data model. Values with
	// custom json.Marshaler or encoding.TextMarshaler behavior are rejected so
	// hidden encoder output cannot alter approval or persistence identity.
	Metadata map[string]any `json:"metadata,omitempty"`
	// ImageAttachmentResolver is a trusted, Run-scoped dependency used to
	// materialize transient URLs for historical image attachments. It is never
	// accepted from callers or persisted with the Run input.
	ImageAttachmentResolver ImageAttachmentResolver `json:"-"`
	// TrustedContext is host-authored, current external state that must be
	// available to the model for this Run but must not be accepted from API
	// callers or persisted as a user-authored transcript message.
	TrustedContext string `json:"-"`
}

type RuntimeConfig struct {
	Model           Model
	Operations      *OperationRegistry
	MCPInstructions string
	Skills          *skills.SkillSet
	Policy          OperationPolicy
	Executor        OperationExecutor
	Verifier        ResultVerifier
	Approver        Approver
	ApprovalResumer ApprovalResumer
	RunStore        RunStore
	Executions      ExecutionStore
	EventSink       EventSink
	ContextWindow   *ContextWindowConfig

	MaxIterations        int
	MaxCallsPerTurn      int
	SessionLeaseTTL      time.Duration
	LeaseRenewalInterval time.Duration
	CleanupTimeout       time.Duration
	Now                  func() time.Time
	// NewID may be invoked concurrently by concurrent Run calls. Implementations
	// must be concurrency-safe and must not rely on Runtime holding a host-facing
	// serialization lock while the callback executes. A custom factory requires
	// RunStore as durable uniqueness authority after in-flight claims retire.
	NewID func() string
}

type runtimeIdentityClaim struct {
	kind  string
	scope *runtimeIdentityScope
}

type runtimeIdentityScope struct {
	claims map[string]struct{}
}

type runtimeIdentityScopeContextKey struct{}

type Runtime struct {
	model            Model
	operations       *OperationRegistry
	mcp              *localMCP
	policy           OperationPolicy
	verifier         ResultVerifier
	approver         Approver
	approvalResumer  ApprovalResumer
	runStore         RunStore
	executions       ExecutionStore
	eventSink        EventSink
	contextWindow    *ContextWindowConfig
	baseInstructions string
	skillSetID       string
	toolSnapshot     []ToolDefinition
	toolSnapshotID   string
	operationSetID   string

	maxIterations        int
	maxCallsPerTurn      int
	sessionLeaseTTL      time.Duration
	leaseRenewalInterval time.Duration
	cleanupTimeout       time.Duration
	now                  func() time.Time
	newID                func() string
	identityMu           sync.Mutex
	assignedIdentities   map[string]runtimeIdentityClaim
}

type Result struct {
	RunID          string
	SessionID      string
	Status         RunStatus
	LastResponseID string
	Output         string
}

type agentState struct {
	sessionID              string
	sessionReady           bool
	lease                  *leaseGuard
	seenCallIDs            map[string]struct{}
	seenResponseIDs        map[string]struct{}
	seenProviderItemIDs    map[string]struct{}
	operationBatchCount    uint64
	planCallID             string
	planExecutionID        string
	lastResponseID         string
	createdAt              time.Time
	instructions           string
	persistentInstructions string
	transcript             []ModelInputItem
	checkpoint             *ContextCheckpoint
	pendingApproval        *PendingApprovalCommit
	resumedApproval        *PendingApprovalCommit
	pendingApprovalDigest  string
	loadedOperationSetID   string
}

type modelCallError struct {
	modelCallID string
	cause       error
}

type operationCallError struct {
	callID      string
	executionID string
	attemptID   string
	cause       error
}

func (e *operationCallError) Error() string { return e.cause.Error() }
func (e *operationCallError) Unwrap() error { return e.cause }

func correlateOperationError(callID, executionID, attemptID string, cause error) error {
	if cause == nil {
		return nil
	}
	if _, ok := errors.AsType[*operationCallError](cause); ok {
		return cause
	}
	return &operationCallError{callID: callID, executionID: executionID, attemptID: attemptID, cause: cause}
}

func (e *modelCallError) Error() string { return e.cause.Error() }
func (e *modelCallError) Unwrap() error { return e.cause }

func correlateModelCallError(modelCallID string, cause error) error {
	if cause == nil || modelCallID == "" {
		return cause
	}
	if _, ok := errors.AsType[*modelCallError](cause); ok {
		return cause
	}
	return &modelCallError{modelCallID: modelCallID, cause: cause}
}

type runtimeSettings struct {
	maxIterations        int
	maxCallsPerTurn      int
	sessionLeaseTTL      time.Duration
	leaseRenewalInterval time.Duration
	cleanupTimeout       time.Duration
	now                  func() time.Time
	newID                func() string
	contextWindow        *ContextWindowConfig
}

type runtimeCatalog struct {
	operations       *OperationRegistry
	mcp              *localMCP
	baseInstructions string
	skillSetID       string
	toolSnapshot     []ToolDefinition
	operationSetID   string
}

func validateRuntimeDependencies(cfg RuntimeConfig) error {
	if err := validateUTF8Boundary("runtime configuration", struct {
		MCPInstructions string
	}{MCPInstructions: cfg.MCPInstructions}); err != nil {
		return err
	}
	if isNilDependency(cfg.Model) {
		return errors.New("agent: model is required")
	}
	if cfg.NewID != nil && isNilDependency(cfg.RunStore) {
		return errors.New("agent: custom NewID requires RunStore durable identity authority")
	}
	optionalDependencies := []struct {
		name  string
		value any
	}{
		{name: "operation policy", value: cfg.Policy},
		{name: "operation executor", value: cfg.Executor},
		{name: "result verifier", value: cfg.Verifier},
		{name: "approver", value: cfg.Approver},
		{name: "approval resumer", value: cfg.ApprovalResumer},
		{name: "run store", value: cfg.RunStore},
		{name: "execution store", value: cfg.Executions},
	}
	for _, dependency := range optionalDependencies {
		if dependency.value != nil && isNilDependency(dependency.value) {
			return fmt.Errorf("agent: configured %s is nil", dependency.name)
		}
	}
	if cfg.Skills != nil && ((cfg.Skills.ID() == "") != (cfg.Skills.Len() == 0)) {
		return errors.New("agent: configured SkillSet is invalid")
	}
	return validateContextWindowConfig(cfg.ContextWindow)
}

func normalizeRuntimeSettings(cfg RuntimeConfig) (runtimeSettings, error) {
	settings := runtimeSettings{
		maxIterations:        cfg.MaxIterations,
		maxCallsPerTurn:      cfg.MaxCallsPerTurn,
		sessionLeaseTTL:      cfg.SessionLeaseTTL,
		leaseRenewalInterval: cfg.LeaseRenewalInterval,
		cleanupTimeout:       cfg.CleanupTimeout,
		now:                  cfg.Now,
		newID:                cfg.NewID,
	}
	if settings.maxIterations == 0 {
		settings.maxIterations = defaultMaxIterations
	}
	if settings.maxIterations < 0 {
		return runtimeSettings{}, errors.New("agent: max iterations must be positive")
	}
	if settings.maxCallsPerTurn == 0 {
		settings.maxCallsPerTurn = defaultMaxCallsPerTurn
	}
	if settings.maxCallsPerTurn < 0 {
		return runtimeSettings{}, errors.New("agent: max calls per turn must be positive")
	}
	if settings.now == nil {
		settings.now = time.Now
	}
	if settings.newID == nil {
		settings.newID = randomID
	}
	if settings.sessionLeaseTTL == 0 {
		settings.sessionLeaseTTL = defaultSessionLeaseTTL
	}
	if settings.sessionLeaseTTL < 0 {
		return runtimeSettings{}, errors.New("agent: session lease TTL must be positive")
	}
	if settings.leaseRenewalInterval == 0 {
		settings.leaseRenewalInterval = defaultSessionLeaseRenewal
	}
	if settings.leaseRenewalInterval < 0 || settings.leaseRenewalInterval >= settings.sessionLeaseTTL {
		return runtimeSettings{}, errors.New("agent: lease renewal interval must be positive and shorter than the session lease TTL")
	}
	if settings.cleanupTimeout == 0 {
		settings.cleanupTimeout = defaultDetachedCleanupTimeout
	}
	if settings.cleanupTimeout < 0 {
		return runtimeSettings{}, errors.New("agent: cleanup timeout must be positive")
	}
	if cfg.ContextWindow != nil {
		cloned := *cfg.ContextWindow
		settings.contextWindow = &cloned
	}
	return settings, nil
}

func prepareRuntimeCatalog(cfg RuntimeConfig) (runtimeCatalog, error) {
	catalog := runtimeCatalog{operations: cfg.Operations}
	if catalog.operations == nil {
		catalog.operations = NewOperationRegistry()
	}
	operationSummaries := catalog.operations.Summaries()
	if err := validateRuntimeOperationConfig(operationSummaries, cfg); err != nil {
		return runtimeCatalog{}, err
	}
	if err := catalog.operations.Freeze(); err != nil {
		return runtimeCatalog{}, err
	}
	operationSummaries = catalog.operations.Summaries()
	if err := validateRuntimeOperationConfig(operationSummaries, cfg); err != nil {
		return runtimeCatalog{}, err
	}
	var err error
	catalog.mcp, err = newLocalMCP(
		catalog.operations, cfg.Executor, cfg.MCPInstructions,
	)
	if err != nil {
		return runtimeCatalog{}, err
	}
	skillInstructions := ""
	if cfg.Skills != nil && cfg.Skills.Len() > 0 {
		catalog.skillSetID = cfg.Skills.ID()
		skillInstructions = buildSkillInstructions(cfg.Skills.Skills())
	}
	catalog.baseInstructions = buildBaseInstructions(catalog.mcp.Instructions(), skillInstructions)
	catalog.toolSnapshot = catalog.mcp.Tools()
	catalog.operationSetID = operationSetID(operationSummaries)
	if err := validateUTF8Boundary("runtime catalog", struct {
		Instructions   string
		SkillSetID     string
		OperationSetID string
		Tools          []ToolDefinition
	}{
		Instructions: catalog.baseInstructions, SkillSetID: catalog.skillSetID,
		OperationSetID: catalog.operationSetID, Tools: catalog.toolSnapshot,
	}); err != nil {
		return runtimeCatalog{}, err
	}
	return catalog, nil
}

func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if err := validateRuntimeDependencies(cfg); err != nil {
		return nil, err
	}
	settings, err := normalizeRuntimeSettings(cfg)
	if err != nil {
		return nil, err
	}
	catalog, err := prepareRuntimeCatalog(cfg)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		model:                cfg.Model,
		operations:           catalog.operations,
		mcp:                  catalog.mcp,
		policy:               cfg.Policy,
		verifier:             cfg.Verifier,
		approver:             cfg.Approver,
		approvalResumer:      cfg.ApprovalResumer,
		runStore:             cfg.RunStore,
		executions:           cfg.Executions,
		eventSink:            cfg.EventSink,
		contextWindow:        settings.contextWindow,
		baseInstructions:     catalog.baseInstructions,
		skillSetID:           catalog.skillSetID,
		toolSnapshot:         catalog.toolSnapshot,
		toolSnapshotID:       toolDefinitionsID(catalog.toolSnapshot),
		operationSetID:       catalog.operationSetID,
		maxIterations:        settings.maxIterations,
		maxCallsPerTurn:      settings.maxCallsPerTurn,
		sessionLeaseTTL:      settings.sessionLeaseTTL,
		leaseRenewalInterval: settings.leaseRenewalInterval,
		cleanupTimeout:       settings.cleanupTimeout,
		now:                  settings.now,
		newID:                settings.newID,
		assignedIdentities:   make(map[string]runtimeIdentityClaim),
	}, nil
}

// nextGeneratedID is the single trust boundary for host-provided identity
// factories. The host callback runs without an internal state lock so it can
// participate in host control flow without lock inversion. Claims are scoped
// to one exported Run or reconciliation call and retired after that call ends;
// durable stores remain authoritative for identities retained beyond the call.
func (r *Runtime) nextGeneratedID(ctx context.Context, kind string) (string, error) {
	if r == nil || r.newID == nil {
		return "", fmt.Errorf("agent: generated %s has no identity factory", kind)
	}
	id := r.newID()
	if err := validateRuntimeIdentity(id, "generated "+kind); err != nil {
		return "", err
	}
	if err := r.claimRuntimeIdentity(ctx, id, kind); err != nil {
		return "", err
	}
	return id, nil
}

func (r *Runtime) reserveRunIdentity(ctx context.Context, id string) error {
	if err := validateRuntimeIdentity(id, "run id"); err != nil {
		return err
	}
	if r.runStore == nil {
		return fmt.Errorf("%w: explicit run id %q requires durable RunStore authority", ErrIdentityConflict, id)
	}
	return r.claimRuntimeIdentity(ctx, id, "run id")
}

func validateRuntimeIdentity(id, kind string) error {
	if !utf8.ValidString(id) {
		return fmt.Errorf("agent: %s must be valid UTF-8", kind)
	}
	if err := requireCanonicalIdentity(id, kind); err != nil {
		return fmt.Errorf("agent: invalid runtime identity: %w", err)
	}
	return nil
}

func (r *Runtime) beginIdentityScope(ctx context.Context) (context.Context, *runtimeIdentityScope) {
	scope := &runtimeIdentityScope{claims: make(map[string]struct{})}
	return context.WithValue(ctx, runtimeIdentityScopeContextKey{}, scope), scope
}

func (r *Runtime) releaseIdentityScope(scope *runtimeIdentityScope) {
	if r == nil || scope == nil {
		return
	}
	r.identityMu.Lock()
	defer r.identityMu.Unlock()
	for id := range scope.claims {
		if claim, exists := r.assignedIdentities[id]; exists && claim.scope == scope {
			delete(r.assignedIdentities, id)
		}
	}
	scope.claims = nil
}

func (r *Runtime) claimRuntimeIdentity(ctx context.Context, id, kind string) error {
	scope, ok := ctx.Value(runtimeIdentityScopeContextKey{}).(*runtimeIdentityScope)
	if !ok || scope == nil {
		return fmt.Errorf("agent: %s identity %q has no active runtime scope", kind, id)
	}
	r.identityMu.Lock()
	defer r.identityMu.Unlock()
	if existing, exists := r.assignedIdentities[id]; exists {
		return fmt.Errorf("%w: %s identity %q was already assigned as %s", ErrIdentityConflict, kind, id, existing.kind)
	}
	if r.assignedIdentities == nil {
		r.assignedIdentities = make(map[string]runtimeIdentityClaim)
	}
	r.assignedIdentities[id] = runtimeIdentityClaim{kind: kind, scope: scope}
	scope.claims[id] = struct{}{}
	return nil
}

func validateRuntimeOperationConfig(operations []OperationSummary, cfg RuntimeConfig) error {
	if len(operations) == 0 {
		return nil
	}
	if isNilDependency(cfg.Policy) {
		return errors.New("agent: operation policy is required when operations are registered")
	}
	if isNilDependency(cfg.Executor) {
		return errors.New("agent: operation executor is required when operations are registered")
	}
	hasWriteOperation := false
	for _, operation := range operations {
		if operation.Effect == OperationEffectWrite {
			hasWriteOperation = true
		}
		if operation.Confirmation.Mode == ConfirmationRequired && isNilDependency(cfg.Verifier) {
			return fmt.Errorf("%w: operation %q", ErrVerifierRequired, operation.Name)
		}
	}
	if hasWriteOperation && isNilDependency(cfg.Executions) {
		return ErrExecutionStoreRequired
	}
	return nil
}
