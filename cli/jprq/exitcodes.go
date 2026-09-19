package jprq

// Exit-code contract between the jprq agent (cli/) and whatever consumes
// its process exit status (a human today, the Kubernetes controller
// eventually). Defined once, here — do not duplicate these numbers or
// stderr-prefix strings elsewhere in the module.
//
// Source of truth: docs/DESIGN.md §7.2, transcribed verbatim. Do not
// invent or renumber a code without updating DESIGN.md first.
//
// Matching is on the prefix before the format verb only: upstream's
// events.WriteError has a bug (see events.go) where fmt.Sprintf(message,
// args) renders %s as a literal Go slice, e.g. "subdomain is busy: [mysub],
// try another one" instead of "...: mysub, ...". The prefixes below match
// either form.

const (
	ExitOK                     = 0  // clean SIGTERM/SIGINT shutdown
	ExitUnexpected             = 1  // unclassified error; retryable
	ExitSubdomainBusy          = 10 // ErrPrefixSubdomainBusy; drives §9.3 collision handling
	ExitCNAMEBusy              = 11 // ErrPrefixCNAMEBusy; reserved, unreachable in v1 (JPRQ_CNAME unset)
	ExitAccountTunnelLimit     = 12 // ErrPrefixTunnelsLimitReached; terminal
	ExitAuthFailed             = 13 // ErrPrefixAuthFailed; terminal
	ExitInvalidSubdomain       = 14 // ErrPrefixInvalidSubdomain; should be unreachable given §5.2 validation
	ExitNotAllowlisted         = 15 // ErrPrefixInviteOnly; terminal
	ExitConfigFailure          = 20 // bootstrap/config/token-file/remote-config failure; retryable
	ExitEventServerUnreachable = 21 // could not dial the event server; retryable
	ExitEventStreamDropped     = 22 // established event connection read error; expected per F2, drives restart
)

// Stderr message prefixes (DESIGN.md F6/§7.2). Match on these, never on
// the full formatted string, per the WriteError bug noted above.
const (
	ErrPrefixSubdomainBusy       = "subdomain is busy:"
	ErrPrefixCNAMEBusy           = "cname is busy:"
	ErrPrefixTunnelsLimitReached = "tunnels limit reached for"
	ErrPrefixAuthFailed          = "authentication failed"
	ErrPrefixInvalidSubdomain    = "invalid subdomain"
	ErrPrefixInviteOnly          = "jprq is now invite-only service"
)
