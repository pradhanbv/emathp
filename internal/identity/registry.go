package identity

// IssuerRegistration maps one trusted issuer to its tenant and the roles
// its groups carry. This is control-plane state (changes at human
// timescales), stubbed here as an in-memory map.
type IssuerRegistration struct {
	Tenant     string
	GroupRoles map[string]string            // group ID -> role name
	GroupAttrs map[string]map[string]string // group ID -> resolved attributes (e.g. region)
}

type Registry struct {
	byIssuer map[string]IssuerRegistration
}

func NewRegistry() *Registry {
	return &Registry{byIssuer: make(map[string]IssuerRegistration)}
}

func (r *Registry) Register(issuer string, reg IssuerRegistration) *Registry {
	r.byIssuer[issuer] = reg
	return r
}

func (r *Registry) ByIssuer(issuer string) (IssuerRegistration, bool) {
	reg, ok := r.byIssuer[issuer]
	return reg, ok
}
