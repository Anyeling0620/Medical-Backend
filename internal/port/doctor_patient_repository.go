package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/doctorpatient"
)

// DoctorPatientRepository 描述「医生本人患者」只读查询能力（契约 §6.10）。
//
// 与挂号仓储分开声明：本视图的主表是就诊卡，聚合口径（挂号次数、最近就诊日期、
// 最近支付状态）属于医生工作台的展示需求，不应混进挂号域的持久化接口。
type DoctorPatientRepository interface {
	// ListDoctorPatients 按过滤条件分页返回该医生接诊过的患者，并返回总数。
	//
	// DoctorID 必须由调用方从登录主体推导（契约 §1.2）；offset/limit 由调用方
	// 按契约 §1.4 校验 page/pageSize 后换算。
	ListDoctorPatients(
		ctx context.Context,
		f doctorpatient.Filter,
		offset, limit int,
	) ([]doctorpatient.Patient, int64, error)
}
