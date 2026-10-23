package model

import (
	"fmt"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Schema from v1.0.0-rc.44, before mapped-model and cache-token statistics.
type releasedQuotaData struct {
	Id        int    `json:"id"`
	UserID    int    `json:"user_id" gorm:"index"`
	Username  string `json:"username" gorm:"index:idx_qdt_model_user_name,priority:2;size:64;default:''"`
	ModelName string `json:"model_name" gorm:"index:idx_qdt_model_user_name,priority:1;size:64;default:''"`
	CreatedAt int64  `json:"created_at" gorm:"bigint;index:idx_qdt_created_at,priority:2"`
	UseGroup  string `json:"use_group" gorm:"index;size:64;default:''"`
	TokenID   int    `json:"token_id" gorm:"index;default:0"`
	ChannelID int    `json:"channel_id" gorm:"index;default:0"`
	NodeName  string `json:"node_name" gorm:"index;size:64;default:''"`
	TokenUsed int    `json:"token_used" gorm:"default:0"`
	Count     int    `json:"count" gorm:"default:0"`
	Quota     int    `json:"quota" gorm:"default:0"`
}

func (releasedQuotaData) TableName() string { return "quota_data" }

func TestQuotaDataDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			dsn := os.Getenv(dialect.env)
			if dialect.env == "" {
				previousPath := common.SQLitePath
				common.SQLitePath = t.TempDir() + "/quota.db"
				t.Cleanup(func() { common.SQLitePath = previousPath })
				dsn = "local"
			} else if dsn == "" {
				t.Skip(dialect.env + " is not configured")
			}
			t.Setenv("QUOTA_DATA_TEST_DSN", dsn)
			db, typ, err := chooseDB("QUOTA_DATA_TEST_DSN", false)
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			previousDB, previousType, previousMemory := DB, common.MainDatabaseType(), common.MemoryCacheEnabled
			DB, common.MemoryCacheEnabled = db, false
			common.SetMainDatabaseType(typ)
			initCol()
			t.Cleanup(func() {
				DB, common.MemoryCacheEnabled = previousDB, previousMemory
				common.SetMainDatabaseType(previousType)
				initCol()
				require.NoError(t, sqlDB.Close())
			})
			versionSQL := "SELECT version()"
			if dialect.kind == "sqlite" {
				versionSQL = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionSQL).Scan(&version).Error)
			t.Logf("database version: %s", version)
			for _, upgrade := range []bool{false, true} {
				t.Run(fmt.Sprintf("upgrade=%v", upgrade), func(t *testing.T) {
					require.NoError(t, db.Migrator().DropTable(&QuotaData{}, &Channel{}))
					t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(&QuotaData{}, &Channel{})) })
					require.NoError(t, db.AutoMigrate(&Channel{}))
					expected := QuotaData{Id: 1, UserID: 7, Username: "legacy", ModelName: "alias", CreatedAt: 3600, UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a", TokenUsed: 99, Count: 2, Quota: 77}
					if upgrade {
						require.NoError(t, db.AutoMigrate(&releasedQuotaData{}))
						require.NoError(t, db.Create(&releasedQuotaData{Id: 1, UserID: 7, Username: "legacy", ModelName: "alias", CreatedAt: 3600, UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a", TokenUsed: 99, Count: 2, Quota: 77}).Error)
					} else {
						require.NoError(t, db.AutoMigrate(&QuotaData{}))
						expected.UpstreamModelName, expected.CacheReadTokens, expected.CacheCreationTokens = "served-model", 5, 7
						require.NoError(t, db.Create(&expected).Error)
					}
					originalIndexes, err := db.Migrator().GetIndexes(&QuotaData{})
					require.NoError(t, err)
					recorder := &migrationSQLRecorder{}
					for run := range 2 {
						recorder.reset()
						require.NoError(t, db.Session(&gorm.Session{Logger: recorder}).AutoMigrate(&QuotaData{}))
						if run == 1 {
							assert.Empty(t, recorder.schemaMutations(), "restarting must not repeat schema changes")
						}
					}
					var stored QuotaData
					require.NoError(t, db.First(&stored, 1).Error)
					assert.Equal(t, expected, stored)
					indexes, err := db.Migrator().GetIndexes(&QuotaData{})
					require.NoError(t, err)
					byName := make(map[string]gorm.Index, len(indexes))
					for _, index := range indexes {
						byName[index.Name()] = index
					}
					for _, original := range originalIndexes {
						preserved, ok := byName[original.Name()]
						require.True(t, ok, "existing index %s must remain", original.Name())
						assert.ElementsMatch(t, original.Columns(), preserved.Columns(), original.Name())
						originalUnique, originalKnown := original.Unique()
						preservedUnique, preservedKnown := preserved.Unique()
						assert.Equal(t, originalKnown, preservedKnown, original.Name())
						assert.Equal(t, originalUnique, preservedUnique, original.Name())
					}
					require.NoError(t, db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&QuotaData{}).Error)
					testAdminQuotaDataReportsMappedUpstreamModel(t)
				})
			}
		})
	}
}

func seedFlowQuotaData(t *testing.T, quotaData QuotaData) {
	t.Helper()
	require.NoError(t, DB.Create(&quotaData).Error)
}

func seedFlowLookupData(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Create(&Channel{Id: 1, Name: "east"}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 2, Name: "west"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 11, UserId: 1, Key: "sk-primary", Name: "primary"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 22, UserId: 2, Key: "sk-backup", Name: "backup"}).Error)
	require.NoError(t, DB.Delete(&Token{Id: 11}).Error)
}

func TestGetFlowQuotaDataUsesQuotaDataRoleSpecificDimensions(t *testing.T) {
	truncateTables(t)
	seedFlowLookupData(t)

	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 1,
		CreatedAt: 1000,
		Count:     2,
		Quota:     100,
		TokenUsed: 40,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 1,
		CreatedAt: 1100,
		Count:     1,
		Quota:     50,
		TokenUsed: 20,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 2,
		CreatedAt: 1200,
		Count:     1,
		Quota:     25,
		TokenUsed: 10,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    2,
		Username:  "bob",
		NodeName:  "node-b",
		TokenID:   22,
		UseGroup:  "default",
		ModelName: "gpt-b",
		ChannelID: 1,
		CreatedAt: 1300,
		Count:     3,
		Quota:     70,
		TokenUsed: 30,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		ModelName: "legacy",
		CreatedAt: 1400,
		Count:     99,
		Quota:     999,
		TokenUsed: 999,
	})

	rootRows, err := GetFlowQuotaData(900, 2000, "", 0, common.RoleRootUser)
	require.NoError(t, err)
	require.Len(t, rootRows, 3)
	// Token 11 was soft-deleted, so its name is intentionally left empty for the
	// frontend to render a localized "deleted (id)" label instead.
	require.Equal(t, FlowQuotaData{
		UserID:      1,
		Username:    "alice",
		NodeName:    "node-a",
		TokenID:     11,
		TokenName:   "",
		UseGroup:    "vip",
		ChannelID:   1,
		ChannelName: "east",
		ModelName:   "gpt-a",
		TokenUsed:   60,
		Count:       3,
		Quota:       150,
	}, *rootRows[0])
	// A token that still exists resolves to its current name.
	require.Equal(t, 22, rootRows[1].TokenID)
	require.Equal(t, "backup", rootRows[1].TokenName)

	adminRows, err := GetFlowQuotaData(900, 2000, "alice", 0, common.RoleAdminUser)
	require.NoError(t, err)
	require.Len(t, adminRows, 2)
	require.Equal(t, 0, adminRows[0].TokenID)
	require.Empty(t, adminRows[0].TokenName)
	require.Empty(t, adminRows[0].NodeName)
	require.Equal(t, "alice", adminRows[0].Username)
	require.Equal(t, "vip", adminRows[0].UseGroup)
	require.Equal(t, "east", adminRows[0].ChannelName)
	require.Equal(t, 150, adminRows[0].Quota)

	selfRows, err := GetFlowQuotaData(900, 2000, "", 1, common.RoleCommonUser)
	require.NoError(t, err)
	require.Len(t, selfRows, 1)
	require.Empty(t, selfRows[0].Username)
	require.Equal(t, 0, selfRows[0].ChannelID)
	require.Empty(t, selfRows[0].ChannelName)
	require.Empty(t, selfRows[0].TokenName)
	require.Equal(t, "vip", selfRows[0].UseGroup)
	require.Equal(t, 175, selfRows[0].Quota)
}

func TestLogQuotaDataSplitsRowsByUseGroupTokenChannelAndNode(t *testing.T) {
	truncateTables(t)
	CacheQuotaDataLock.Lock()
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()

	LogQuotaData(QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3661,
		UseGroup:  "vip",
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     100,
		TokenUsed: 40,
	})
	LogQuotaData(QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3700,
		UseGroup:  "vip",
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     50,
		TokenUsed: 20,
	})
	LogQuotaData(QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3700,
		UseGroup:  "default",
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     25,
		TokenUsed: 10,
	})

	SaveQuotaDataCache()

	var rows []QuotaData
	require.NoError(t, DB.Order("quota DESC").Find(&rows).Error)
	require.Len(t, rows, 2)
	require.Equal(t, int64(3600), rows[0].CreatedAt)
	require.Equal(t, "vip", rows[0].UseGroup)
	require.Equal(t, 11, rows[0].TokenID)
	require.Equal(t, 1, rows[0].ChannelID)
	require.Equal(t, "node-a", rows[0].NodeName)
	require.Equal(t, 2, rows[0].Count)
	require.Equal(t, 150, rows[0].Quota)
	require.Equal(t, 60, rows[0].TokenUsed)
	require.Equal(t, "default", rows[1].UseGroup)
	require.Equal(t, 25, rows[1].Quota)
}

func TestAdminQuotaDataReportsMappedUpstreamModel(t *testing.T) {
	truncateTables(t)
	testAdminQuotaDataReportsMappedUpstreamModel(t)
}

func testAdminQuotaDataReportsMappedUpstreamModel(t *testing.T) {
	t.Helper()
	CacheQuotaDataLock.Lock()
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()

	direct := QuotaDataLogParams{
		UserID: 1, Username: "alice", ModelName: "gemini", CreatedAt: 3661,
		UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a", Quota: 100, TokenUsed: 40,
		CacheReadTokens: 10, CacheCreationTokens: 20,
	}
	// The retry lands on a channel that maps gemini to deepseek.
	mapped := direct
	mapped.ChannelID = 2
	mapped.UpstreamModelName = "deepseek"
	mapped.Quota = 30
	mapped.CacheReadTokens = 3
	mapped.CacheCreationTokens = 5

	LogQuotaData(direct)
	LogQuotaData(mapped)
	SaveQuotaDataCache()
	// A second flush must add to the mapped row instead of creating a new one.
	LogQuotaData(mapped)
	SaveQuotaDataCache()

	var stored int64
	require.NoError(t, DB.Model(&QuotaData{}).Count(&stored).Error)
	require.Equal(t, int64(2), stored)

	quotaByModel := func(rows []*QuotaData) map[string]int {
		result := make(map[string]int)
		for _, row := range rows {
			result[row.ModelName] += row.Quota
		}
		return result
	}

	adminRows, err := GetAllQuotaDates(0, 7200, "")
	require.NoError(t, err)
	require.Equal(t, map[string]int{"gemini": 100, "deepseek": 60}, quotaByModel(adminRows))

	adminUserRows, err := GetAllQuotaDates(0, 7200, "alice")
	require.NoError(t, err)
	require.Equal(t, map[string]int{"gemini": 100, "deepseek": 60}, quotaByModel(adminUserRows))
	for _, rows := range [][]*QuotaData{adminRows, adminUserRows} {
		for _, row := range rows {
			if row.ModelName == "deepseek" {
				assert.EqualValues(t, 6, row.CacheReadTokens)
				assert.EqualValues(t, 10, row.CacheCreationTokens)
			} else {
				assert.EqualValues(t, 10, row.CacheReadTokens)
				assert.EqualValues(t, 20, row.CacheCreationTokens)
			}
		}
	}

	// Users keep seeing the model they requested; the mapping stays hidden.
	selfRows, err := GetQuotaDataByUserId(1, 0, 7200)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"gemini": 160}, quotaByModel(selfRows))
	require.Len(t, selfRows, 1)
	assert.EqualValues(t, 16, selfRows[0].CacheReadTokens)
	assert.EqualValues(t, 30, selfRows[0].CacheCreationTokens)

	flowRows, err := GetFlowQuotaData(0, 7200, "", 0, common.RoleAdminUser)
	require.NoError(t, err)
	require.Len(t, flowRows, 2)
	require.Equal(t, "gemini", flowRows[0].ModelName)
	require.Equal(t, 1, flowRows[0].ChannelID)
	require.Equal(t, "deepseek", flowRows[1].ModelName)
	require.Equal(t, 2, flowRows[1].ChannelID)
}
