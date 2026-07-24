package redisguard

// T-13 / D-2 — the reachability taxonomy and refusal (pack 20260721; the Go
// twin of Python's redis_preflight.classify_redis_error / reachability_error,
// ported as a RULE). A DECLARED Redis that cannot be reached or authenticated
// is a REACHABILITY refusal, never an infrastructure verdict: the
// topology/eviction/scripting/version checks never ran.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// RedisReachableOK is the published capability value on a verified PING —
// the Python REACHABLE_OK literal, shared cross-runtime vocabulary.
const RedisReachableOK = "on (PING verified)"

// ErrRedisUnreachable is the typed D-2 startup refusal sentinel.
var ErrRedisUnreachable = errors.New("quota: declared Redis unreachable or unauthenticated")

// UnreachableError carries the classified kind ('auth'|'acl'|'unreachable'|'error').
type UnreachableError struct {
	Kind string
	msg  string
}

func (e *UnreachableError) Error() string { return e.msg }
func (e *UnreachableError) Unwrap() error { return ErrRedisUnreachable }

// ClassifyRedisError sorts an error into the cross-runtime taxonomy
// (Python's classify_redis_error, same kinds, same rule):
//
//	'auth'        — failed AUTHENTICATION (NOAUTH/WRONGPASS): nothing was
//	                checked at all; does not heal by waiting;
//	'acl'         — NOPERM: this specific command denied — a genuine ABSENT
//	                signal where operator assertion flags legitimately apply;
//	'unreachable' — network: the D-2 retry budget applies to this kind ONLY;
//	''            — anything else.
//
// Type-based first; message tokens as the disclosed-heuristic fallback
// (D-14 #4 / F-1 sweep: the pinned go-redis v9.6.1 surfaces server error
// lines VERBATIM — ParseErrorReply → RedisError(line[1:]) — so the
// NOAUTH/WRONGPASS/NOPERM prefixes are the server's own text, not a client
// rewrite; net-level failures are matched by TYPE before any string).
func ClassifyRedisError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	// Message classes first for redis server replies (they arrive as generic
	// redis errors, not typed ones).
	if strings.Contains(s, "noperm") {
		return "acl"
	}
	for _, tok := range []string{"noauth", "wrongpass", "invalid password",
		"invalid username-password", "authentication required"} {
		if strings.Contains(s, tok) {
			return "auth"
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return "unreachable"
	}
	for _, tok := range []string{"connection refused", "connection reset", "broken pipe",
		"timed out", "i/o timeout", "no route to host", "network is unreachable",
		"no such host", "name or service not known", "dial tcp", "eof",
		"closed network connection", "context deadline exceeded"} {
		if strings.Contains(s, tok) {
			return "unreachable"
		}
	}
	return ""
}

// ReachabilityError builds the typed D-2 refusal — Python's
// reachability_error text, plus the retry lever the operator can act on.
func ReachabilityError(kind, detail, urlDisplay, source string, budgetSeconds float64) *UnreachableError {
	where := ""
	if urlDisplay != "" {
		where = " " + urlDisplay
	}
	src := ""
	if source != "" {
		src = " (declared by " + source + ")"
	}
	verb := "REACH"
	if kind == "auth" || kind == "acl" {
		verb = "AUTHENTICATE to"
	}
	return &UnreachableError{Kind: kind, msg: fmt.Sprintf(
		"ab0t-quota could not %s the DECLARED Redis%s%s — %s: %s. This is NOT a topology "+
			"verdict: the topology/eviction/scripting/version checks never ran. Boot retried the "+
			"unreachable kind within the D-2 budget (storage.connect_retry_seconds=%.0f; 0 = fail "+
			"immediately) before refusing; auth failures never retry. Remedy: fix the credential or "+
			"reachability condition (storage.redis_url / storage.redis_password / URL userinfo, "+
			"network path). The *_confirmed_* assertion flags would NOT help and are not the remedy — "+
			"they exist for a REACHABLE Redis whose signals cannot be probed.",
		verb, where, src, kind, detail, budgetSeconds)}
}
