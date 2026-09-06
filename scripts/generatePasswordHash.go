package scripts

import (
	"fmt"
	"golang.org/x/crypto/bcrypt"
)

func generateHashPassword() {
	pwd := "yourpassword"
	str, _ := bcrypt.GenerateFromPassword([]byte(pwd), bcrypt.DefaultCost)
	fmt.Print(string(str))

}
