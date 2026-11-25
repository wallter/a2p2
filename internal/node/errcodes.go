package node

// Refusal codes: every error the local API and the agent tools return
// carries one, with whether repeating the same call later can succeed
// without changing it (PROTOCOL section 7). This is the only list;
// a code is never renamed once shipped, and a new one is added here and
// in the protocol together. The sentence beside the code says what to do.
const (
	// ErrorInvalid: the request is malformed or names something that
	// cannot be (a bad label, a bad argument). The default code.
	ErrorInvalid = "invalid_request"
	// ErrorNotFound: no such room, message, member or request here.
	ErrorNotFound = "not_found"
	// ErrorNotAllowed: the caller may not do this (not addressed, its own
	// message, not an admin, the local room).
	ErrorNotAllowed = "not_allowed"
	// ErrorNotShown: a handled report for a message whose text the agent
	// has not been shown yet; reading it first makes the same call succeed.
	ErrorNotShown = "not_shown"
	// ErrorAlreadyDone: it was done before (a report already sent, a
	// request already decided, an invite already used).
	ErrorAlreadyDone = "already_done"
	// ErrorNotRegistered: the agent is not registered here; registering
	// again (the MCP shim does so by itself) makes the call succeed.
	ErrorNotRegistered = "not_registered"
	// ErrorTooLarge: the message is too long or names too many addressees.
	ErrorTooLarge = "too_large"
	// ErrorNetworkingOff: refused because networking is off or outbound
	// only here; only this machine's person can change that.
	ErrorNetworkingOff = "networking_off"
	// ErrorNotCaughtUp is ReasonNotCaughtUp: signing needs the person's
	// explicit second approval (not_caught_up_ok).
	ErrorNotCaughtUp = ReasonNotCaughtUp
	// ErrorTermsChanged: an approval of a send with files carries no
	// terms or not the request's current ones; nothing is sent and the
	// request stays pending. Showing it again gives the terms to approve.
	ErrorTermsChanged = "terms_changed"
	// ErrorRateLimited: another machine is limiting requests; try later.
	ErrorRateLimited = "rate_limited"
	// ErrorUnavailable: the daemon could not be reached (reported by the
	// MCP shim and clients, never by the daemon itself).
	ErrorUnavailable = "unavailable"
	// ErrorInternal: the daemon failed (HTTP 500).
	ErrorInternal = "internal"
)

// errorRetryable is every code with whether the same call may succeed if
// repeated later unchanged.
var errorRetryable = map[string]bool{
	ErrorInvalid:       false,
	ErrorNotFound:      false,
	ErrorNotAllowed:    false,
	ErrorNotShown:      false,
	ErrorAlreadyDone:   false,
	ErrorNotRegistered: true,
	ErrorTooLarge:      false,
	ErrorNetworkingOff: false,
	ErrorNotCaughtUp:   false,
	ErrorTermsChanged:  false,
	ErrorRateLimited:   true,
	ErrorUnavailable:   true,
	ErrorInternal:      true,
}

// ErrorCodes lists every code, in the order above.
var ErrorCodes = []string{ErrorInvalid, ErrorNotFound, ErrorNotAllowed, ErrorNotShown, ErrorAlreadyDone,
	ErrorNotRegistered, ErrorTooLarge, ErrorNetworkingOff, ErrorNotCaughtUp, ErrorTermsChanged, ErrorRateLimited, ErrorUnavailable,
	ErrorInternal}

// Retryable reports whether repeating a call refused with code may succeed
// without changing it. An unknown code is not retryable.
func Retryable(code string) bool { return errorRetryable[code] }

// CodeOf is the code of a user error: its own, or invalid_request.
func (e *UserError) CodeOf() string {
	if e.Code == "" {
		return ErrorInvalid
	}
	return e.Code
}
