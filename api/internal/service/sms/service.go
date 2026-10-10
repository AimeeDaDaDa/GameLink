package sms

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gamelink/internal/service/external"
)

// Provider defines SMS provider interface
type Provider interface {
	SendCode(ctx context.Context, phone, code string) error
	SendNotification(ctx context.Context, phone, templateID string, params map[string]string) error
}

// Service handles SMS operations
type Service struct {
	config *external.Config
}

// NewService creates SMS service
func NewService(cfg *external.Config) *Service {
	return &Service{config: cfg}
}

var (
	// ErrSMSDisabled SMS service is disabled
	ErrSMSDisabled = errors.New("sms service is disabled")
	// ErrUnsupportedProvider unsupported SMS provider
	ErrUnsupportedProvider = errors.New("unsupported sms provider")
)

// SendCode sends verification code
func (s *Service) SendCode(ctx context.Context, phone, code string) error {
	if !s.config.SMS.Enabled {
		return ErrSMSDisabled
	}

	provider, err := s.getProvider()
	if err != nil {
		return err
	}

	return provider.SendCode(ctx, phone, code)
}

// SendNotification sends templated notification
func (s *Service) SendNotification(ctx context.Context, phone, templateID string, params map[string]string) error {
	if !s.config.SMS.Enabled {
		return ErrSMSDisabled
	}

	provider, err := s.getProvider()
	if err != nil {
		return err
	}

	return provider.SendNotification(ctx, phone, templateID, params)
}

func (s *Service) getProvider() (Provider, error) {
	switch s.config.SMS.Provider {
	case "tencent":
		return &TencentProvider{
			SecretID:  s.config.SMS.AccessKey,
			SecretKey: s.config.SMS.SecretKey,
			SignName:  s.config.SMS.SignName,
		}, nil
	case "aliyun":
		return &AliyunProvider{
			AccessKey: s.config.SMS.AccessKey,
			SecretKey: s.config.SMS.SecretKey,
			SignName:  s.config.SMS.SignName,
		}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, s.config.SMS.Provider)
	}
}

// maskPhone 手机号脱敏：保留前 3 位与后 4 位，中间以星号代替，避免完整手机号进入日志。
func maskPhone(phone string) string {
	s := strings.TrimPrefix(phone, "+86")
	s = strings.TrimPrefix(s, "86")
	s = strings.TrimSpace(s)
	if len(s) <= 7 {
		return "***"
	}
	return s[:3] + "****" + s[len(s)-4:]
}
