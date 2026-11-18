package controller

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readRelayPayloadArchive(t *testing.T, path string) []model.RelayPayload {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	decoder, err := zstd.NewReader(file)
	require.NoError(t, err)
	defer decoder.Close()
	var rows []model.RelayPayload
	scanner := bufio.NewScanner(decoder)
	scanner.Buffer(nil, 64<<20)
	for scanner.Scan() {
		var row model.RelayPayload
		require.NoError(t, common.Unmarshal(scanner.Bytes(), &row))
		rows = append(rows, row)
	}
	require.NoError(t, scanner.Err())
	return rows
}

// Only whole UTC days older than the retention leave the database, and only
// after every one of their rows is in that day's archive file.
func TestRelayPayloadArchiveMovesOldDaysToFiles(t *testing.T) {
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			dsn := ""
			if dialect.env != "" {
				if dsn = os.Getenv(dialect.env); dsn == "" {
					t.Skipf("%s not configured", dialect.env)
				}
			}
			db := modelManagementDB(t, dialect.kind, dsn)
			require.NoError(t, db.AutoMigrate(&model.RelayPayload{}))
			at := func(s string) int64 {
				ts, err := time.Parse(time.DateTime, s)
				require.NoError(t, err)
				return ts.Unix()
			}
			seed := []model.RelayPayload{
				{RequestId: "old-1", ChannelId: 3, CreatedAt: at("2026-11-09 00:00:00"), RequestContentType: "application/json",
					RequestBody: `{"input":"<b>a & b</b>\n中文"}`, ResponseBody: "data: {\"x\":1}\n\n"},
				{RequestId: "old-2", ChannelId: 3, CreatedAt: at("2026-11-09 23:59:59"), RequestBody: "two"},
				{RequestId: "old-3", ChannelId: 4, CreatedAt: at("2026-11-10 23:59:59"), RequestBody: "three"},
				{RequestId: "keep-1", ChannelId: 4, CreatedAt: at("2026-11-11 00:00:00"), RequestBody: "kept"},
				{RequestId: "keep-2", ChannelId: 4, CreatedAt: at("2026-11-18 06:00:00"), RequestBody: "kept"},
			}
			for i := range seed {
				require.NoError(t, model.SaveRelayPayload(&seed[i]))
			}
			dir := filepath.Join(t.TempDir(), "relay-archive")
			now := time.Date(2026, 11, 18, 7, 30, 0, 0, time.UTC)

			summary, err := archiveRelayPayloads(context.Background(), dir, 7, now)
			require.NoError(t, err)
			assert.Equal(t, "2026-11-11", summary.Before)
			require.Len(t, summary.Days, 2)
			assert.Equal(t, "relay-payloads-2026-11-09.jsonl.zst", summary.Days[0].File)
			assert.Equal(t, 2, summary.Days[0].Rows)
			assert.Equal(t, 1, summary.Days[1].Rows)
			assert.Equal(t, seed[:2], readRelayPayloadArchive(t, filepath.Join(dir, summary.Days[0].File)))
			assert.Equal(t, seed[2:3], readRelayPayloadArchive(t, filepath.Join(dir, summary.Days[1].File)))
			var left []string
			require.NoError(t, db.Model(&model.RelayPayload{}).Order("id").Pluck("request_id", &left).Error)
			assert.Equal(t, []string{"keep-1", "keep-2"}, left)

			summary, err = archiveRelayPayloads(context.Background(), dir, 7, now)
			require.NoError(t, err)
			assert.Empty(t, summary.Days)

			// A day archived again never overwrites its earlier file.
			late := model.RelayPayload{RequestId: "old-4", CreatedAt: at("2026-11-09 12:00:00"), RequestBody: "four"}
			require.NoError(t, model.SaveRelayPayload(&late))
			summary, err = archiveRelayPayloads(context.Background(), dir, 7, now)
			require.NoError(t, err)
			require.Len(t, summary.Days, 1)
			assert.Equal(t, "relay-payloads-2026-11-09.2.jsonl.zst", summary.Days[0].File)
			assert.Equal(t, []model.RelayPayload{late}, readRelayPayloadArchive(t, filepath.Join(dir, summary.Days[0].File)))
			assert.Len(t, readRelayPayloadArchive(t, filepath.Join(dir, "relay-payloads-2026-11-09.jsonl.zst")), 2)
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Len(t, entries, 3, "no temp files left behind")
		})
	}
}
