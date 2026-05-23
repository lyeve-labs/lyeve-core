package api

// WithRiskBasedMFADisabled suppresses risk-based MFA escalation for a test that
// drives login without a device history. Enrolled MFA is still enforced. See
// the field comment on AuthHandler.
//
// It lives here so it exists only when compiling tests. A setter that turns off
// an authentication control does not belong in a shipped binary, where one
// wiring line would disable it in silence.
func (h *AuthHandler) WithRiskBasedMFADisabled(v bool) {
	h.riskBasedMFADisabled = v
}
