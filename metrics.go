package agentruntime

import "strconv"

// RuntimeMetric is a dependency-neutral metric sample derived from one Event.
// Counters use positive deltas; token values are reported once per completed
// model call. Attributes contain only stable identifiers and enumerations.
type RuntimeMetric struct {
	Name       string            `json:"name"`
	Unit       string            `json:"unit,omitempty"`
	Value      int64             `json:"value"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type MetricSink func(RuntimeMetric)

// MetricsEventSink converts core Runtime events into a minimal stable metric
// vocabulary. It contains MetricSink panics so telemetry cannot fail a Run.
func MetricsEventSink(sink MetricSink) EventSink {
	if sink == nil || isNilDependency(sink) {
		return nil
	}
	emit := func(metric RuntimeMetric) {
		defer func() { _ = recover() }()
		if metric.Attributes != nil {
			attributes := make(map[string]string, len(metric.Attributes))
			for key, value := range metric.Attributes {
				attributes[key] = value
			}
			metric.Attributes = attributes
		}
		sink(metric)
	}
	return func(event Event) {
		attributes := metricAttributes(event)
		switch event.Type {
		case EventModelStarted:
			emit(RuntimeMetric{Name: "agentruntime.model.iterations", Unit: "{call}", Value: 1, Attributes: attributes})
		case EventModelCompleted:
			if event.InputTokens > 0 {
				emit(RuntimeMetric{Name: "agentruntime.model.input_tokens", Unit: "{token}", Value: int64(event.InputTokens), Attributes: attributes})
			}
			if event.OutputTokens > 0 {
				emit(RuntimeMetric{Name: "agentruntime.model.output_tokens", Unit: "{token}", Value: int64(event.OutputTokens), Attributes: attributes})
			}
			if event.TotalTokens > 0 {
				emit(RuntimeMetric{Name: "agentruntime.model.total_tokens", Unit: "{token}", Value: int64(event.TotalTokens), Attributes: attributes})
			}
		case EventSessionLeaseRenewed:
			emit(RuntimeMetric{Name: "agentruntime.session.lease_renewals", Unit: "{renewal}", Value: 1, Attributes: attributes})
		case EventReconciliationCompleted, EventReconciliationFailed:
			attributes["outcome"] = "completed"
			if event.Type == EventReconciliationFailed {
				attributes["outcome"] = "failed"
			}
			emit(RuntimeMetric{Name: "agentruntime.operation.reconciliations", Unit: "{reconciliation}", Value: 1, Attributes: attributes})
		case EventRunCompleted, EventRunWaitingUser, EventRunFailed, EventRunInterrupted, EventRunCancelled:
			attributes["status"] = string(event.Type)
			emit(RuntimeMetric{Name: "agentruntime.runs", Unit: "{run}", Value: 1, Attributes: attributes})
		}
	}
}

func metricAttributes(event Event) map[string]string {
	attributes := make(map[string]string, 5)
	if event.Operation != "" {
		attributes["operation"] = event.Operation
	}
	if event.Reconciliation != "" {
		attributes["reconciliation"] = event.Reconciliation
	}
	if event.ErrorCode != "" {
		attributes["error_code"] = event.ErrorCode
	}
	if event.LeaseGeneration != 0 {
		attributes["lease_generation"] = strconv.FormatUint(event.LeaseGeneration, 10)
	}
	return attributes
}

// CoreUIEvent selects the sanitized events needed for run, approval, and
// reconciliation status surfaces. Audit-only payloads never cross this view.
func CoreUIEvent(event Event) (SanitizedEvent, bool) {
	switch event.Type {
	case EventRunStarted, EventRunWaitingUser, EventRunCompleted, EventRunFailed,
		EventRunInterrupted, EventRunCancelled, EventApprovalRequested,
		EventApprovalCompleted, EventApprovalFailed, EventReconciliationStarted,
		EventReconciliationCompleted, EventReconciliationFailed:
		return SanitizeEvent(event), true
	default:
		return SanitizedEvent{}, false
	}
}
