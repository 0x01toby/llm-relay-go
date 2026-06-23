package responsesconv

import "fmt"

// sprintf is a thin alias so call sites read cleanly.
func sprintf(format string, args ...interface{}) string { return fmt.Sprintf(format, args...) }

// RequestResult mirrors the TS ResponsesChatCompatRequestResult discriminated
// union. Exactly one of Body or Error is meaningful, indicated by OK.
type RequestResult struct {
	OK           bool
	Body         string // valid when OK
	RequestModel string // valid when OK
	Error        CompatError
}

// RequestOptions mirrors ResponsesChatCompatRequestOptions.
type RequestOptions struct {
	TargetURL string
}
