-- 医生账号开通脚本（手工执行，仅用于开发/测试环境；仓库内不保存 init.sql）。
--
-- 背景：管理端登录 POST /api/v1/mis/auth/login 只校验 mis_user 的 username/password/status，
-- 权限来自「角色 -> 权限」映射，因此「以医生身份登录管理端」需要三部分数据：
--   1) mis_user 一行：password 必须是 bcrypt 散列，status = 1；
--   2) mis_user.ref_id = doctor.id：这是「我的患者」接口（GET /api/v1/mis/doctor/patients，
--      契约 §1.2、§6.10）唯一的医生身份依据，库中无外键，只能按该约定关联；
--   3) mis_user_role 绑定「医生」角色（按 role_name = '医生' 解析，该角色已具备 REGISTRATION:SELECT）。
--
-- 用法：把以下三个值替换为真实取值后执行（本脚本可重复执行，不会重复建号或重复绑角色）：
--   :username       登录用户名（≤50 字符，与 mis_user.username 同长度上限）
--   :doctor_id      hospital.doctor.id（绑定的医生档案编号，可通过 SELECT id, name FROM hospital.doctor 查询）
--   :password_hash  bcrypt 散列（cost 10）。仓库里的 scripts/generatePasswordHash.go 目前不是
--                   main 包（`go run ./scripts` 会报 "is not a main package"），可用 htpasswd
--                   或任意 bcrypt 工具生成；散列以 $ 开头，传给 psql 时必须用单引号包住。
--
-- 示例（本机没有 psql 客户端时可用 docker 里的 psql，参数经 psql 变量传入）：
--   docker run --rm -i postgres:16-alpine psql "postgres://root:密码@主机:5432/hospital?sslmode=disable" `
--     -v username=doctor01 -v doctor_id=1 -v password_hash='$2a$10$xxxx' `
--     -f - < scripts/seed_doctor_account.sql
--
-- 注意：mis_user.id / mis_user_role.id 在库中没有序列，这里用 MAX(id) + 1 取值；
-- 该写法只适用于测试环境手工执行，禁止用于生产环境的并发开户。

BEGIN;

-- 1) 不存在同名账号时新建；已存在时保持原密码不变（只补 profile）。
INSERT INTO hospital.mis_user (
    id, username, password, name, sex, dept_id, job, ref_id, status, create_time
)
SELECT
    (SELECT COALESCE(MAX(id), 0) + 1 FROM hospital.mis_user),
    :'username',
    :'password_hash',
    (SELECT name FROM hospital.doctor WHERE id = :doctor_id),
    (SELECT sex FROM hospital.doctor WHERE id = :doctor_id),
    NULL,
    (SELECT job FROM hospital.doctor WHERE id = :doctor_id),
    :doctor_id,
    1,
    CURRENT_DATE
WHERE NOT EXISTS (
    SELECT 1 FROM hospital.mis_user WHERE username = :'username'
);

-- 2) 账号已存在时补齐医生绑定与启用状态（不覆盖已有密码）。
UPDATE hospital.mis_user
SET ref_id = :doctor_id,
    status = 1,
    name = COALESCE(name, (SELECT name FROM hospital.doctor WHERE id = :doctor_id)),
    job = COALESCE(job, (SELECT job FROM hospital.doctor WHERE id = :doctor_id))
WHERE username = :'username';

-- 3) 绑定「医生」角色（mis_role.id = 1）；已绑定则不重复插入。
INSERT INTO hospital.mis_user_role (id, user_id, role_id)
SELECT
    (SELECT COALESCE(MAX(id), 0) + 1 FROM hospital.mis_user_role),
    (SELECT id FROM hospital.mis_user WHERE username = :'username'),
    1
WHERE NOT EXISTS (
    SELECT 1
    FROM hospital.mis_user_role ur
    JOIN hospital.mis_user u ON u.id = ur.user_id
    WHERE u.username = :'username' AND ur.role_id = 1
);

-- 4) 结果自检：登录前可用该查询确认账号已启用、已绑定医生并已具备 REGISTRATION:SELECT。
SELECT u.id, u.username, u.ref_id AS doctor_id, d.name AS doctor_name, u.status,
       ARRAY(
           SELECT DISTINCT p.permission_code
           FROM hospital.mis_user_role ur
           JOIN hospital.mis_role_permission rp ON rp.role_id = ur.role_id
           JOIN hospital.mis_permission p ON p.id = rp.permission_id
           WHERE ur.user_id = u.id
           ORDER BY p.permission_code
       ) AS permissions
FROM hospital.mis_user u
LEFT JOIN hospital.doctor d ON d.id = u.ref_id
WHERE u.username = :'username';

COMMIT;
