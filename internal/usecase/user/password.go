package user

import (
	"crypto/md5"
	"encoding/hex"
)

// LegacyPasswordHash mirrors the existing Java password format:
// MD5(MD5(raw)[:6] + raw + MD5(raw)[len-3:]).
// It is retained for compatibility with existing records. New systems should
// prefer a slow password hash such as bcrypt or Argon2id.
func LegacyPasswordHash(raw string) string {
	first := md5Hex(raw)
	temp := first[:6] + raw + first[len(first)-3:]
	return md5Hex(temp)
}

func md5Hex(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}
