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
	// DoctorID 是医生账号绑定的 doctor.id（取自 mis_user.ref_id），
	// 非医生账号为 null。管理端据此判断是否展示医生工作台（契约 §3.1、§6.10）。
	DoctorID *int64 `json:"doctorId"`
}
