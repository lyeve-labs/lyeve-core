package api

// documentDeviceLogin writes the five device sign-in routes: two public ones
// the device calls and three a signed-in admin decides with.
func documentDeviceLogin(paths map[string]openAPIPath) {
	both := []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}
	admins := []string{"admin", "super_admin"}
	tag := []string{"Admin / Auth"}
	code := openAPIParameter{Name: "user_code", In: "path", Required: true, Schema: map[string]any{"type": "string", "example": "WDJB-MJHT"}}
	body := func(example map[string]any) *openAPIRequestBody {
		return &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{
			"application/json": {Schema: map[string]any{"type": "object", "example": example}},
		}}
	}
	set := func(path, method string, op *openAPIOperation) {
		if paths[path] == nil {
			paths[path] = openAPIPath{}
		}
		if paths[path][method] == nil {
			paths[path][method] = op
		}
	}
	decided := map[string]any{
		"200": map[string]any{"description": "OK"},
		"401": map[string]any{"description": "Unauthorized"},
		"403": map[string]any{"description": "Forbidden: not a signed-in session, not an admin, or on approval the password or MFA code was missing or wrong"},
		"404": map[string]any{"description": "No pending sign-in for this code: reason is unknown, expired or decided"},
		"409": map[string]any{"description": "The sign-in was already approved or denied"},
		"429": map[string]any{"description": "Rate limited"},
	}

	set("/api/admin/auth/device", "post", &openAPIOperation{
		Tags: tag, Summary: "Start a device sign-in",
		Description: "Opens a sign-in for a command line tool or another device without a browser. Answers a device_code the device keeps and polls with, a user_code of the form XXXX-XXXX a person enters on verification_uri while signed in to the admin, and verification_uri_complete with the code filled in. verification_uri is the console's device page on LYEVE_CONSOLE_URL, or on the console dev server outside production when that is unset. In production without LYEVE_CONSOLE_URL the request is refused with 503. The request expires in 600 seconds; poll no more often than interval seconds. Rate limited per address, and one address holds at most ten open requests.",
		RequestBody: body(map[string]any{"client_name": "cli on build-box"}),
		Responses: map[string]any{
			"200": map[string]any{"description": "device_code, user_code, verification_uri, verification_uri_complete, expires_in, interval"},
			"400": map[string]any{"description": "Malformed request"},
			"422": map[string]any{"description": "client_name is missing, longer than 64 characters or not printable"},
			"429": map[string]any{"description": "Rate limited, or too many open requests from this address or its IPv6 /64"},
			"503": map[string]any{"description": "The install holds too many open requests; retry after the Retry-After seconds"},
		},
	})
	set("/api/admin/auth/device/token", "post", &openAPIOperation{
		Tags: tag, Summary: "Poll a device sign-in",
		Description: "Exchanges the device_code for a session once a person approves it. Until then it answers 400 with error set to authorization_pending, slow_down (polled sooner than the interval; the interval grows by five seconds and is returned), access_denied, expired_token (expired, or already exchanged) or invalid_grant (unknown code). On approval it answers 200 with user, token (a session JWT with the engine's normal expiry, the approver's roles in the approved tenant and their token version), token_type, expires_in and expires_at, once. There is no refresh token. Rate limited per address.",
		RequestBody: body(map[string]any{"device_code": "<device_code>"}),
		Responses: map[string]any{
			"200": map[string]any{"description": "A session"},
			"400": map[string]any{"description": "authorization_pending, slow_down, access_denied, expired_token, invalid_grant or invalid_request"},
			"429": map[string]any{"description": "Rate limited"},
		},
	})
	set("/api/admin/auth/device/{user_code}", "get", requiringRoles(&openAPIOperation{
		Tags: tag, Summary: "Read a pending device sign-in", Security: both,
		Description: "What a person is about to approve: the client name as the device sent it, the address the request came from, when it was made and when it expires; and what approving hands over: tenant_id, the tenant the caller's session acts in, roles, the roles the session would carry, and session_expires_in, its lifetime in seconds. approver_ip and same_address set the caller's own address beside the requester's. Session only: an API key or an admin token is refused. The code is read in any case, with or without the dash.",
		Parameters:  []openAPIParameter{code},
		Responses:   decided,
	}, admins...))
	set("/api/admin/auth/device/{user_code}/approve", "post", requiringRoles(&openAPIOperation{
		Tags: tag, Summary: "Approve a device sign-in", Security: both,
		Description: "Binds the sign-in to the caller and the tenant their session acts in. The caller confirms with mfa_code when the account has MFA enrolled and with password otherwise; wrong answers count toward the account's lockouts and a locked account answers 429. The device's next poll receives a session, unless the caller's sessions end first. Session only: an API key or an admin token is refused. One decision per request.",
		Parameters:  []openAPIParameter{code},
		RequestBody: body(map[string]any{"password": "your-password"}),
		Responses:   decided,
	}, admins...))
	set("/api/admin/auth/device/{user_code}/deny", "post", requiringRoles(&openAPIOperation{
		Tags: tag, Summary: "Deny a device sign-in", Security: both,
		Description: "Refuses the sign-in; the device's next poll is answered access_denied. Session only: an API key or an admin token is refused. One decision per request.",
		Parameters:  []openAPIParameter{code},
		Responses:   decided,
	}, admins...))
}
