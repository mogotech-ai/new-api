package model

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// SIWC imports contain refresh credentials, which must stay out of debug SQL logs.
func channelCredentialDB(channels ...Channel) *gorm.DB {
	for _, channel := range channels {
		if common.IsSIWCCredential(channel.Key) {
			return DB.Session(&gorm.Session{Logger: DB.Logger.LogMode(logger.Silent)})
		}
	}
	return DB
}

// AcquireOAuthCredentialLease serializes one registration/account across nodes.
func AcquireOAuthCredentialLease(sessionKey string, owner string, until int64) (string, bool, error) {
	digest := sha256.Sum256([]byte(sessionKey))
	name := fmt.Sprintf("siwc_%x", digest[:24])
	ok, _, err := acquireSystemTaskLock(name, name, owner, common.GetTimestamp(), until)
	return name, ok, err
}

func SaveChannelCredentialWithLease(ctx context.Context, channelID int, previous, replacement, lease, owner string) error {
	held := DB.Model(&SystemTaskLock{}).Select("1").Where("task_id = ? AND locked_by = ? AND locked_until >= ?", lease, owner, common.GetTimestamp())
	result := channelCredentialDB(Channel{Key: previous}).WithContext(ctx).Model(&Channel{}).
		Where(map[string]any{"id": channelID, "key": previous}).
		Where("EXISTS (?)", held).
		Update("key", replacement)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrSystemTaskLockLost
	}
	return nil
}
