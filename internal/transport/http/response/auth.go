package response

type LoginResponse struct {
	User        UserResponse `json:"user"`
	Permissions []string     `json:"permissions"`
}

type UserResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}
