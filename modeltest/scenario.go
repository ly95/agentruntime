package modeltest

import (
	"strings"
	"testing"

	agent "github.com/ly95/agentruntime"
)

// Scenario is one opaque member of the fixed model conformance corpus.
//
// Name is a stable selection key for adapter-owned fixtures. PayloadMarker is a
// non-secret canary for provider-private raw payload or error detail; it must
// never enter semantic text, refusal, tool arguments, identifiers, error codes,
// or ModelBinding. Binding and pre-canceled fixtures do not contact a provider
// and need not inject the canary. Scenario values are immutable and created only
// by this package.
type Scenario interface {
	Name() string
	PayloadMarker() string
	modeltestScenario()
}

// Factory constructs an isolated bound model adapter for scenario. The factory
// may register fixture cleanup with t. It must return a non-nil
// agentruntime.BoundModel; nil and typed-nil values are rejected. It must
// support every scenario and must not skip tests or select behavior through
// capability flags. The concurrency scenario invokes Complete twice on the same
// returned model.
type Factory func(t *testing.T, scenario Scenario) agent.BoundModel

// Stable names in the fixed v1 corpus. A Factory switches on these names to
// select its own provider protocol fixture.
const (
	ScenarioV1Binding                    = "v1/binding"
	ScenarioV1SuccessText                = "v1/success/text"
	ScenarioV1SuccessRefusal             = "v1/success/refusal"
	ScenarioV1SuccessReasoning           = "v1/success/reasoning"
	ScenarioV1SuccessTool                = "v1/success/tool"
	ScenarioV1SuccessStream              = "v1/success/stream"
	ScenarioV1SuccessUsage               = "v1/success/usage"
	ScenarioV1SuccessReplay              = "v1/success/replay"
	ScenarioV1ConcurrencyBoundModel      = "v1/concurrency/bound_model"
	ScenarioV1CancelPreCanceled          = "v1/cancel/pre_canceled"
	ScenarioV1CancelAfterResponseStarted = "v1/cancel/after_response_started"
	ScenarioV1ErrorAuthentication        = "v1/error/authentication"
	ScenarioV1ErrorQuota                 = "v1/error/quota"
	ScenarioV1ErrorRateLimit             = "v1/error/rate_limit"
	ScenarioV1ErrorRejected              = "v1/error/rejected"
	ScenarioV1ErrorTransient             = "v1/error/transient"
	ScenarioV1InvalidUnknownOutput       = "v1/invalid/unknown_output"
	ScenarioV1InvalidDuplicateOutput     = "v1/invalid/duplicate_output"
	ScenarioV1InvalidReorderedOutput     = "v1/invalid/reordered_output"
	ScenarioV1InvalidContradictoryID     = "v1/invalid/contradictory_identity"
	ScenarioV1InvalidPartialCompletion   = "v1/invalid/partial_completion"
)

type scenarioKind uint8

const (
	scenarioBinding scenarioKind = iota
	scenarioSuccessText
	scenarioSuccessRefusal
	scenarioSuccessReasoning
	scenarioSuccessTool
	scenarioSuccessStream
	scenarioSuccessUsage
	scenarioSuccessReplay
	scenarioConcurrencyBoundModel
	scenarioCancelPreCanceled
	scenarioCancelAfterResponseStarted
	scenarioErrorAuthentication
	scenarioErrorQuota
	scenarioErrorRateLimit
	scenarioErrorRejected
	scenarioErrorTransient
	scenarioInvalidUnknownOutput
	scenarioInvalidDuplicateOutput
	scenarioInvalidReorderedOutput
	scenarioInvalidContradictoryID
	scenarioInvalidPartialCompletion
)

type corpusScenario struct {
	name   string
	marker string
	kind   scenarioKind
}

func (scenario corpusScenario) Name() string          { return scenario.name }
func (scenario corpusScenario) PayloadMarker() string { return scenario.marker }
func (corpusScenario) modeltestScenario()             {}

func newCorpusScenario(name string, kind scenarioKind) corpusScenario {
	return corpusScenario{
		name:   name,
		marker: "modeltest_private_payload::" + strings.ReplaceAll(name, "/", "_") + "::v1_canary",
		kind:   kind,
	}
}

var v1Corpus = [...]corpusScenario{
	newCorpusScenario(ScenarioV1Binding, scenarioBinding),
	newCorpusScenario(ScenarioV1SuccessText, scenarioSuccessText),
	newCorpusScenario(ScenarioV1SuccessRefusal, scenarioSuccessRefusal),
	newCorpusScenario(ScenarioV1SuccessReasoning, scenarioSuccessReasoning),
	newCorpusScenario(ScenarioV1SuccessTool, scenarioSuccessTool),
	newCorpusScenario(ScenarioV1SuccessStream, scenarioSuccessStream),
	newCorpusScenario(ScenarioV1SuccessUsage, scenarioSuccessUsage),
	newCorpusScenario(ScenarioV1SuccessReplay, scenarioSuccessReplay),
	newCorpusScenario(ScenarioV1ConcurrencyBoundModel, scenarioConcurrencyBoundModel),
	newCorpusScenario(ScenarioV1CancelPreCanceled, scenarioCancelPreCanceled),
	newCorpusScenario(ScenarioV1CancelAfterResponseStarted, scenarioCancelAfterResponseStarted),
	newCorpusScenario(ScenarioV1ErrorAuthentication, scenarioErrorAuthentication),
	newCorpusScenario(ScenarioV1ErrorQuota, scenarioErrorQuota),
	newCorpusScenario(ScenarioV1ErrorRateLimit, scenarioErrorRateLimit),
	newCorpusScenario(ScenarioV1ErrorRejected, scenarioErrorRejected),
	newCorpusScenario(ScenarioV1ErrorTransient, scenarioErrorTransient),
	newCorpusScenario(ScenarioV1InvalidUnknownOutput, scenarioInvalidUnknownOutput),
	newCorpusScenario(ScenarioV1InvalidDuplicateOutput, scenarioInvalidDuplicateOutput),
	newCorpusScenario(ScenarioV1InvalidReorderedOutput, scenarioInvalidReorderedOutput),
	newCorpusScenario(ScenarioV1InvalidContradictoryID, scenarioInvalidContradictoryID),
	newCorpusScenario(ScenarioV1InvalidPartialCompletion, scenarioInvalidPartialCompletion),
}
