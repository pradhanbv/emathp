package identity

import "errors"

// ErrUnauthenticated maps to the UNAUTHENTICATED error code (HTTP 401): the
// caller did not present a credential this gateway can turn into a
// principal - no `Bearer` prefix, unparseable claims, or an issuer that is
// not a registered tenant.
//
// Kept distinct from ErrPrincipalUnresolved because the two are different
// failures wearing similar words. This one is the caller's to fix, and a
// client should re-authenticate rather than retry. Returning 503 for it -
// which this package did until the pre-submission audit - tells a client to
// back off and retry, and pages an on-call for what is a routine unauthenticated
// request.
var ErrUnauthenticated = errors.New("unauthenticated: no resolvable principal")

// ErrPrincipalUnresolved maps to the PRINCIPAL_UNRESOLVED error code (HTTP
// 503): the credential was fine, but the attribute source needed to finish
// resolving the principal was unreachable and no cached copy was fresh
// enough to use - so we fail closed rather than proceed with partial
// attributes (DESIGN.md ADR-011).
//
// No code path reaches this yet: the prototype resolves attributes from the
// in-process issuer registry, which cannot be unavailable. It is defined
// here so the 503 case has a name the day attribute resolution becomes a
// network call.
var ErrPrincipalUnresolved = errors.New("principal unresolved: attribute source unavailable")
