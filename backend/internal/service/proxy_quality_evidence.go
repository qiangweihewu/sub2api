package service

import "errors"

const (
	ProxyEvidenceConfig      = "config"
	ProxyEvidenceObserved    = "observed"
	ProxyEvidenceUnavailable = "unavailable"
)

const (
	ProxyQualityReasonProbeUnavailable       = "probe_unavailable"
	ProxyQualityReasonProbeFailed            = "probe_failed"
	ProxyQualityReasonProbeCanceled          = "probe_canceled"
	ProxyQualityReasonRequestCanceled        = "request_canceled"
	ProxyQualityReasonRequestBuildFailed     = "request_build_failed"
	ProxyQualityReasonRequestFailed          = "request_failed"
	ProxyQualityReasonResponseReadFailed     = "response_read_failed"
	ProxyQualityReasonCloudflareChallenge    = "cloudflare_challenge"
	ProxyQualityReasonTargetRateLimited      = "target_rate_limited"
	ProxyQualityReasonUnexpectedHTTPStatus   = "unexpected_http_status"
	ProxyQualityReasonInvalidProbeResponse   = "invalid_probe_response"
	ProxyQualityReasonProbeResponseUncertain = "probe_response_uncertain"
)

// ProxyProbeDiagnosticError marks errors that prevent the exit probe from
// establishing evidence about proxy connectivity. Its public Error string is
// deliberately fixed so wrapped HTTP/client details cannot leak credentials.
type ProxyProbeDiagnosticError struct {
	ReasonCode string
}

func (e *ProxyProbeDiagnosticError) Error() string {
	return "proxy probe could not establish evidence"
}

func NewProxyProbeDiagnosticError(reasonCode string) error {
	switch reasonCode {
	case ProxyQualityReasonRequestBuildFailed,
		ProxyQualityReasonResponseReadFailed,
		ProxyQualityReasonUnexpectedHTTPStatus,
		ProxyQualityReasonInvalidProbeResponse,
		ProxyQualityReasonProbeResponseUncertain:
		return &ProxyProbeDiagnosticError{ReasonCode: reasonCode}
	default:
		return &ProxyProbeDiagnosticError{ReasonCode: ProxyQualityReasonProbeResponseUncertain}
	}
}

func proxyProbeUnknownReason(err error) string {
	if err == nil {
		return ""
	}
	var diagnosticErr *ProxyProbeDiagnosticError
	if errors.As(err, &diagnosticErr) && proxyQualityKnownReason(diagnosticErr.ReasonCode) {
		return diagnosticErr.ReasonCode
	}
	return ""
}

func ProxyProbeDiagnosticErrorReason(err error) string {
	return proxyProbeUnknownReason(err)
}

func proxyQualityKnownReason(reasonCode string) bool {
	switch reasonCode {
	case ProxyQualityReasonProbeUnavailable,
		ProxyQualityReasonProbeFailed,
		ProxyQualityReasonProbeCanceled,
		ProxyQualityReasonRequestCanceled,
		ProxyQualityReasonRequestBuildFailed,
		ProxyQualityReasonRequestFailed,
		ProxyQualityReasonResponseReadFailed,
		ProxyQualityReasonCloudflareChallenge,
		ProxyQualityReasonTargetRateLimited,
		ProxyQualityReasonUnexpectedHTTPStatus,
		ProxyQualityReasonInvalidProbeResponse,
		ProxyQualityReasonProbeResponseUncertain:
		return true
	default:
		return false
	}
}

func proxyQualityUnknownItem(target, reasonCode, message string) ProxyQualityCheckItem {
	return ProxyQualityCheckItem{
		Target:         target,
		Status:         "unknown",
		EvidenceSource: ProxyEvidenceUnavailable,
		ReasonCode:     reasonCode,
		Message:        message,
	}
}

func proxyQualitySafeProbeError(err error) (reasonCode, message string, unknown bool) {
	if err == nil {
		return "", "", false
	}
	if reasonCode := proxyProbeUnknownReason(err); reasonCode != "" {
		return reasonCode, "代理探测结果不可判定", true
	}
	return ProxyQualityReasonProbeFailed, "代理出口探测失败", false
}

func proxyProbeUnknownForContext(ctxErr error, err error) (reasonCode string, unknown bool) {
	if ctxErr != nil {
		return ProxyQualityReasonProbeCanceled, true
	}
	if reasonCode := proxyProbeUnknownReason(err); reasonCode != "" {
		return reasonCode, true
	}
	return "", false
}
