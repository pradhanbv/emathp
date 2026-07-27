package identity

import "errors"

// ErrPrincipalUnresolved maps to the PRINCIPAL_UNRESOLVED error code: the
// token's issuer is not a registered tenant.
var ErrPrincipalUnresolved = errors.New("principal unresolved: issuer not registered")
