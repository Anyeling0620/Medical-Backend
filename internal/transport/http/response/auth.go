package response

type LoginResponse struct {
	User            UserResponse `json:"user"`
	Permissions     []string     `json:"permissions"`
	AccessExpiresAt string       `json:"accessExpiresAt"`
}

type UserResponse struct {
	ID           int64   `json:"id"`
	Username     string  `json:"username"`
	Name         *string `json:"name"`
	DepartmentID *int64  `json:"departmentId"`
	Job          *string `json:"job"`
}
