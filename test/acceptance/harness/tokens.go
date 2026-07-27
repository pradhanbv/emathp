package harness

// Placeholder identities. Cycle 1 replaces these with real unverified-JWT
// fixtures under testdata/tokens that exercise issuer->tenant derivation.
func tokenFor(role string) string {
	switch role {
	case "admin":
		return "admin-token"
	default:
		return "support-token"
	}
}
