package user

// User contains the identity data needed by the authentication flow.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Status       int16
	// Name/DepartmentID/Job 为登录响应中的用户资料字段，
	// 使用指针以区分“数据库为空”和“字符串为空”两种语义。
	Name         *string
	DepartmentID *int64
	Job          *string
}
