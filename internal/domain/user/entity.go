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
	// RefID 是 mis_user.ref_id（关联业务编号）。医生账号用它绑定 doctor.id：
	// 管理端「我的患者」等医生视角接口按本字段确定数据范围（契约 §6.10），
	// 未绑定（为 nil）的账号不允许访问医生视角数据。
	RefID *int64
}
