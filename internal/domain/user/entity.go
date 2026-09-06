package user

// User contains the identity data needed by the authentication flow.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
}
