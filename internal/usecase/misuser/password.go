package misuser

import (
	"golang.org/x/crypto/bcrypt"
)

func comparePassword(hash, raw string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(raw))
}
