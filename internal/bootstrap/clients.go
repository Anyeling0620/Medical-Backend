package bootstrap

import (
	"context"
	"time"

	rmq "github.com/apache/rocketmq-clients/golang/v5"
	"github.com/apache/rocketmq-clients/golang/v5/credentials"
	"github.com/minio/minio-go/v7"
	minioCredentials "github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"

	"Medical-Web-Backend/internal/config"
)

type dependencyState struct {
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

type clients struct {
	redis    *redis.Client
	minio    *minio.Client
	producer rmq.Producer
}

func connectDependencies(cfg config.Config) (clients, map[string]dependencyState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	states := make(map[string]dependencyState, 4)
	var result clients

	redisClient := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Username: cfg.Redis.Username, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		states["redis"] = dependencyState{Error: err.Error()}
		_ = redisClient.Close()
	} else {
		result.redis = redisClient
		states["redis"] = dependencyState{Connected: true}
	}

	minioClient, err := minio.New(cfg.MinIO.Endpoint, &minio.Options{Creds: minioCredentials.NewStaticV4(cfg.MinIO.AccessKey, cfg.MinIO.SecretKey, ""), Secure: cfg.MinIO.UseSSL})
	if err != nil {
		states["minio"] = dependencyState{Error: err.Error()}
	} else if _, err = minioClient.ListBuckets(ctx); err != nil {
		states["minio"] = dependencyState{Error: err.Error()}
	} else {
		result.minio = minioClient
		states["minio"] = dependencyState{Connected: true}
	}

	producer, err := rmq.NewProducer(&rmq.Config{
		Endpoint:  cfg.RocketMQ.Endpoint,
		NameSpace: cfg.RocketMQ.Namespace,
		Credentials: &credentials.SessionCredentials{
			AccessKey:    cfg.RocketMQ.AccessKey,
			AccessSecret: cfg.RocketMQ.SecretKey,
		},
	}, rmq.WithTopics(cfg.RocketMQ.Topic))
	if err != nil {
		states["rocketmq"] = dependencyState{Error: err.Error()}
	} else {
		rmq.EnableSsl = false
		startResult := make(chan error, 1)
		go func() { startResult <- producer.Start() }()
		select {
		case err = <-startResult:
			if err != nil {
				states["rocketmq"] = dependencyState{Error: err.Error()}
			} else {
				result.producer = producer
				states["rocketmq"] = dependencyState{Connected: true}
			}
		case <-time.After(10 * time.Second):
			states["rocketmq"] = dependencyState{Error: "gRPC startup timed out after 10s"}
			go func() { _ = producer.GracefulStop() }()
		}
	}

	return result, states
}
