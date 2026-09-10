// Package auth 定义跨端共享的认证基础概念。
//
// 管理端（Web 后台）与患者端（微信小程序）共用同一套签名算法、Redis 会话存储
// 以及刷新/撤销逻辑，仅通过 realm 区分令牌的主体归属，避免复制两套认证实现。
package auth

// Realm 表示访问令牌所属的认证域。
//
// 令牌的解释规则与 realm 绑定：realm=mis 的令牌主体只能是 mis_user，
// realm=patient 的令牌主体只能是 patient_user。令牌 realm 与所访问接口要求的
// realm 不一致时，一律按无效令牌处理，不得回退到另一域解释同一个令牌。
type Realm string

const (
	// RealmMis 管理端认证域，对应 mis_user、角色与权限编码。
	RealmMis Realm = "mis"
	// RealmPatient 患者端认证域，对应 patient_user 及其就诊卡。
	RealmPatient Realm = "patient"
)

// Valid 判断 realm 是否为契约允许的取值。
func (r Realm) Valid() bool {
	return r == RealmMis || r == RealmPatient
}

// String 让 realm 能直接参与日志与错误信息拼接。
func (r Realm) String() string {
	return string(r)
}
