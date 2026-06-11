package handlerledger

import "fmt"

// SkipError is a sentinel returned by Context.Skip. The dispatcher
// records status=skipped with the reason.
type SkipError struct{ Reason string }

func (e *SkipError) Error() string { return fmt.Sprintf("handler skipped: %s", e.Reason) }

// SuccessError is a sentinel returned by Context.Success. The dispatcher
// records status=success with the side_effect_id.
//
// Why a sentinel "error" type rather than a return value? Go has no
// equivalent of Python's `return ctx.success(...)` ergonomics — the
// closest match is to overload the error return slot. Handlers can also
// return plain nil; the dispatcher treats that as status=success without
// a side_effect_id.
type SuccessError struct{ SideEffectID string }

func (e *SuccessError) Error() string { return "handler succeeded: " + e.SideEffectID }
