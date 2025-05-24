package logger

import (
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type RateLimiter struct {
	errorLogInterval time.Duration
	lastLogTimes     map[string]time.Time
	mu               sync.Mutex
	logger           *logrus.Logger
}

func NewRateLimiter(errorLogInterval time.Duration, logger *logrus.Logger) *RateLimiter {
	return &RateLimiter{
		errorLogInterval: errorLogInterval,
		lastLogTimes:     make(map[string]time.Time),
		logger:           logger,
	}
}

func (r *RateLimiter) LogError(node, errorType, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := fmt.Sprintf("%s:%s", node, errorType)
	lastLog, exists := r.lastLogTimes[key]
	
	if !exists || time.Since(lastLog) >= r.errorLogInterval {
		r.logger.WithFields(logrus.Fields{
			"node":       node,
			"error_type": errorType,
		}).Error(message)
		r.lastLogTimes[key] = time.Now()
	}
}

func (r *RateLimiter) LogWarning(node, warningType, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := fmt.Sprintf("%s:%s:warning", node, warningType)
	lastLog, exists := r.lastLogTimes[key]
	
	if !exists || time.Since(lastLog) >= r.errorLogInterval {
		r.logger.WithFields(logrus.Fields{
			"node":         node,
			"warning_type": warningType,
		}).Warn(message)
		r.lastLogTimes[key] = time.Now()
	}
}

func SetupLogger(logLevel string) (*logrus.Logger, error) {
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: time.RFC3339,
	})

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		return nil, fmt.Errorf("invalid log level: %w", err)
	}
	logger.SetLevel(level)

	return logger, nil
}