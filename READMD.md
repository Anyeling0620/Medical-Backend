# 项目结构
```
medical-backend/
├── cmd/
│   └── server/
│       └── main.go
│
├── internal/
│   ├── config/
│   │   └── config.go
│   │
│   ├── bootstrap/
│   │   ├── app.go
│   │   ├── clients.go
│   │   └── pgsql.go
│   │
│   ├── domain/
│   │   ├── user/
│   │   │   └── entity.go
│   │   ├── doctor/
│   │   │   └── entity.go
│   │   ├── appointment/
│   │   │   └── entity.go
│   │   └── medical_record/
│   │       └── entity.go
│   │
│   ├── usecase/
│   │   ├── user/
│   │   │   └── service.go
│   │   ├── appointment/
│   │   │   └── service.go
│   │   └── medical_record/
│   │       └── service.go
│   │
│   ├── port/
│   │   ├── user_repository.go
│   │   ├── appointment_repository.go
│   │   ├── cache.go
│   │   ├── object_storage.go
│   │   └── event_bus.go
│   │
│   ├── adapter/
│   │   ├── postgres/
│   │   ├── redis/
│   │   ├── minio/
│   │   └── rocketmq/
│   │
│   ├── transport/
│   │   └── http/
│   │       ├── handler/
│   │       ├── middleware/
│   │       └── router.go
│   │
│   ├── worker/
│   │   └── consumer.go
│   │
│   └── observability/
│       ├── logger.go
│       ├── metrics.go
│       └── tracing.go
│
├── api/
│   └── proto/
│       └── *.proto
│
├── deploy/
│   ├── Dockerfile
│   ├── docker-compose.yaml
│   └── nginx.conf
│
├── scripts/
│   └── postgres/
│
├── go.mod
└── go.sum
```
# 结构设计
| 结构              | 用途                                        |
|-----------------|-------------------------------------------|
| router.go       | 定义接口地址                                    |
| handler/        | 接收和处理 HTTP 请求                             |
| request/        | 定义请求参数                                    |
| response/       | 定义返回格式                                    |
| middleware/     | 处理认证、日志、限流等公共逻辑                           |
| usecase/        | 处理业务流程                                    |
| repo/           | 访问 PostgreSQL 和 Redis |

# 推荐使用的库
| 用途 | 推荐库 | 说明                    |
|------|--------|-----------------------|
| HTTP | github.com/gin-gonic/gin | 和示例项目一致，学习成本低         |
| 配置 | github.com/caarlos0/env/v11 | 直接读取环境变量，简单透明         |
| 日志 | 标准库 log/slog | Go 原生结构化日志            |
| 参数校验 | github.com/go-playground/validator/v10 | Gin 生态常用              |
| Redis | github.com/redis/go-redis/v9 | 官方社区主流客户端             |
| PostgreSQL | github.com/jackc/pgx/v5 | PostgreSQL 驱动与客户端    |
| gRPC | google.golang.org/grpc | 如果 Go 服务需要提供或调用 gRPC  |
| Protobuf | google.golang.org/protobuf | gRPC 消息定义             |
| JWT | github.com/golang-jwt/jwt/v5 | JWT 生成和校验             |
| 密码哈希 | golang.org/x/crypto/bcrypt | 用户密码不要明文保存            |
| UUID | github.com/google/uuid | 生成业务 ID               |
| 测试 | 标准库 testing + github.com/stretchr/testify | 断言和 Mock              |
| 指标 | github.com/prometheus/client_golang | Prometheus 监控         |
| 链路追踪 | go.opentelemetry.io/otel | OpenTelemetry         |
