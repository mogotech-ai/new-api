package model

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf16"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// RelayPayloadBody holds a whole request or response body. It maps to longtext
// on MySQL, where a bare TEXT holds only 64 KiB, and to the unbounded text type
// on PostgreSQL and SQLite.
type RelayPayloadBody string

func (RelayPayloadBody) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "mysql" {
		return "longtext"
	}
	return "text"
}

type RelayPayload struct {
	Id                  int64            `json:"id"`
	RequestId           string           `json:"request_id" gorm:"size:64;uniqueIndex"`
	ChannelId           int              `json:"channel_id" gorm:"index"`
	CreatedAt           int64            `json:"created_at" gorm:"bigint;index"`
	RequestContentType  string           `json:"request_content_type" gorm:"size:128"`
	ResponseContentType string           `json:"response_content_type" gorm:"size:128"`
	RequestBody         RelayPayloadBody `json:"request_body"`
	ResponseBody        RelayPayloadBody `json:"response_body"`
	// The truncated flags are only set on rows written while bodies were
	// capped at 32 KiB; new rows always leave them false.
	RequestBodyTruncated  bool `json:"request_body_truncated"`
	ResponseBodyTruncated bool `json:"response_body_truncated"`
}

func SaveRelayPayload(payload *RelayPayload) error {
	return DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "request_id"}},
		UpdateAll: true,
	}).Create(payload).Error
}

func GetRelayPayload(requestId string) (*RelayPayload, error) {
	payload := &RelayPayload{}
	if err := DB.Where("request_id = ?", requestId).First(payload).Error; err != nil {
		return nil, err
	}
	return payload, nil
}

// RelayPayloadSearchLimit caps a keyword search to the newest matches, so the
// database can stop scanning bodies once it has found enough of them.
const RelayPayloadSearchLimit = 200

var relayPayloadLikeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// SearchRelayPayloadRequestIds returns the request ids of the newest payloads
// in the time range whose request or response body contains keyword. Scope
// "request" or "response" searches one body; anything else searches both.
// Case sensitivity follows the database: PostgreSQL LIKE is case sensitive,
// MySQL and SQLite are not.
func SearchRelayPayloadRequestIds(ctx context.Context, keyword string, scope string, startTimestamp int64, endTimestamp int64, channelId int) ([]string, error) {
	columns := []string{"request_body", "response_body"}
	switch scope {
	case "request":
		columns = columns[:1]
	case "response":
		columns = columns[1:]
	}
	// Some clients (Python's json.dumps by default) send non-ASCII text as
	// \uXXXX escapes, so also match the keyword in that form.
	patterns := []string{keyword}
	var escaped strings.Builder
	for _, unit := range utf16.Encode([]rune(keyword)) {
		if unit < 0x80 {
			escaped.WriteByte(byte(unit))
		} else {
			fmt.Fprintf(&escaped, "\\u%04x", unit)
		}
	}
	if escaped.String() != keyword {
		patterns = append(patterns, escaped.String())
	}

	var conditions []string
	var args []any
	for _, column := range columns {
		for _, pattern := range patterns {
			conditions = append(conditions, column+" LIKE ? ESCAPE '!'")
			args = append(args, "%"+relayPayloadLikeEscaper.Replace(pattern)+"%")
		}
	}
	tx := DB.WithContext(ctx).Model(&RelayPayload{}).
		Where("created_at >= ? AND created_at <= ?", startTimestamp, endTimestamp).
		Where("("+strings.Join(conditions, " OR ")+")", args...)
	if channelId != 0 {
		tx = tx.Where("channel_id = ?", channelId)
	}
	var requestIds []string
	err := tx.Order("created_at desc").Limit(RelayPayloadSearchLimit).Pluck("request_id", &requestIds).Error
	return requestIds, err
}

func countOldRelayPayload(ctx context.Context, targetTimestamp int64) (int64, error) {
	if DB == nil || !DB.Migrator().HasTable(&RelayPayload{}) {
		return 0, nil
	}
	var total int64
	if err := DB.WithContext(ctx).Model(&RelayPayload{}).Where("created_at < ?", targetTimestamp).Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func deleteOldRelayPayloadBatch(ctx context.Context, targetTimestamp int64, limit int) (int64, error) {
	if DB == nil || !DB.Migrator().HasTable(&RelayPayload{}) {
		return 0, nil
	}
	result := DB.WithContext(ctx).Where("created_at < ?", targetTimestamp).Limit(limit).Delete(&RelayPayload{})
	return result.RowsAffected, result.Error
}
