package plugin

// TerminalFailure is returned only when the executor has observed a terminal
// guest process, or a completed process returned an invalid protocol response.
// Transport errors are deliberately not terminal: retry must reconnect to the
// same process rather than duplicate an operation whose outcome is unknown.
type TerminalFailure struct{ Detail string }

func (e *TerminalFailure) Error() string { return e.Detail }
