package user

import (
	"context"
	"errors"
	"strings"

	domainuser "Medical-Web-Backend/internal/domain/user"
	"Medical-Web-Backend/internal/port"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInvalidInput       = errors.New("username and password are required")
)

type Service struct {
	repository port.UserRepository
}

func NewService(repository port.UserRepository) *Service {
	return &Service{repository: repository}
}

// Authenticate applies the compatibility password transformation before the
// repository lookup, matching the Java login query's password comparison.
func (s *Service) Authenticate(ctx context.Context, username, password string) (*domainuser.User, error) {
	username = strings.TrimSpace(username)
	// 用户名或密码不合法
	if username == "" || password == "" {
		return nil, ErrInvalidInput
	}
	// 查库失败
	if s.repository == nil {
		return nil, errors.New("user repository is not configured")
	}
	u, err := s.repository.Authenticate(ctx, username, LegacyPasswordHash(password))
	// 验证不通过（用户名 密码不匹配）
	if err != nil || u == nil {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}
