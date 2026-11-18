package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/klauspost/compress/zstd"
)

// relayPayloadArchiveHandler keeps only the newest RELAY_PAYLOAD_RETENTION_DAYS
// (default 7) whole UTC days of request/response bodies in the database. Each
// older day is written to one zstd file of JSON lines in
// RELAY_PAYLOAD_ARCHIVE_DIR and only then deleted from relay_payloads; usage
// logs are never touched. Read an archive with `zstd -dc <file> | grep ...`.
type relayPayloadArchiveHandler struct{}

const systemTaskTypeRelayPayloadArchive = "relay_payload_archive"

func (relayPayloadArchiveHandler) Type() string { return systemTaskTypeRelayPayloadArchive }

func (relayPayloadArchiveHandler) Enabled() bool {
	return common.GetEnvOrDefaultBool("RELAY_PAYLOAD_ARCHIVE_ENABLED", true)
}

func (relayPayloadArchiveHandler) Interval() time.Duration { return time.Hour }

func (relayPayloadArchiveHandler) NewPayload() any { return nil }

func (relayPayloadArchiveHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	// The docker image's working directory /data is the mounted data volume.
	dir := common.GetEnvOrDefaultString("RELAY_PAYLOAD_ARCHIVE_DIR", "relay-archive")
	days := max(common.GetEnvOrDefault("RELAY_PAYLOAD_RETENTION_DAYS", 7), 1)
	summary, err := archiveRelayPayloads(ctx, dir, days, time.Now())
	status := model.SystemTaskStatusSucceeded
	if err != nil {
		status = model.SystemTaskStatusFailed
		common.SysError(common.LogText("relay payload archive: %v", err))
	}
	finishSystemTaskHandler(task, runnerID, status, summary, err)
}

type relayPayloadArchiveDay struct {
	Day   string `json:"day"`
	File  string `json:"file"`
	Rows  int    `json:"rows"`
	Bytes int64  `json:"bytes"`
}

type relayPayloadArchiveSummary struct {
	Dir    string                   `json:"dir"`
	Before string                   `json:"before"`
	Days   []relayPayloadArchiveDay `json:"days"`
}

const secondsPerDay = 24 * 60 * 60

func archiveRelayPayloads(ctx context.Context, dir string, retentionDays int, now time.Time) (*relayPayloadArchiveSummary, error) {
	cutoff := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -retentionDays)
	summary := &relayPayloadArchiveSummary{Dir: dir, Before: cutoff.Format(time.DateOnly), Days: []relayPayloadArchiveDay{}}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return summary, err
	}
	for previous := int64(-1); ctx.Err() == nil; {
		oldest, ok, err := model.OldestRelayPayloadCreatedAt(ctx)
		if err != nil || !ok || oldest >= cutoff.Unix() {
			return summary, err
		}
		dayStart := oldest - oldest%secondsPerDay
		if dayStart == previous {
			return summary, fmt.Errorf("rows of %d remain after archiving that day", dayStart)
		}
		previous = dayStart
		day, err := archiveRelayPayloadDay(ctx, dir, dayStart)
		if err != nil {
			return summary, err
		}
		summary.Days = append(summary.Days, day)
		common.SysLog(common.LogText("relay payload archive: %s: %d rows to %s (%d bytes)", day.Day, day.Rows, day.File, day.Bytes))
	}
	return summary, ctx.Err()
}

// archiveRelayPayloadDay writes every payload of the UTC day starting at
// dayStart to a new file, makes it durable, and only then deletes those rows.
func archiveRelayPayloadDay(ctx context.Context, dir string, dayStart int64) (relayPayloadArchiveDay, error) {
	result := relayPayloadArchiveDay{Day: time.Unix(dayStart, 0).UTC().Format(time.DateOnly)}
	base := filepath.Join(dir, "relay-payloads-"+result.Day)
	tmp := base + ".jsonl.zst.tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return result, err
	}
	defer os.Remove(tmp) // no-op once renamed
	defer file.Close()
	// One stream per day with a long window: consecutive calls repeat the same
	// prompts, so this is ~10x smaller than the database's per-row compression.
	encoder, err := zstd.NewWriter(file, zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithWindowSize(32<<20), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return result, err
	}
	var ids []int64
	for afterId := int64(0); ; {
		rows, err := model.GetRelayPayloadsCreatedBetween(ctx, dayStart, dayStart+secondsPerDay, afterId, 100)
		if err != nil {
			encoder.Close()
			return result, err
		}
		if len(rows) == 0 {
			break
		}
		for i := range rows {
			line, err := common.Marshal(&rows[i])
			if err == nil {
				_, err = encoder.Write(append(line, '\n'))
			}
			if err != nil {
				encoder.Close()
				return result, err
			}
			ids = append(ids, rows[i].Id)
		}
		afterId = rows[len(rows)-1].Id
	}
	if err := encoder.Close(); err != nil {
		return result, err
	}
	if err := file.Sync(); err != nil {
		return result, err
	}
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	// A day archived again (rerun after a failed delete) gets its own file.
	final := base + ".jsonl.zst"
	for n := 2; ; n++ {
		if _, err := os.Stat(final); os.IsNotExist(err) {
			break
		}
		final = fmt.Sprintf("%s.%d.jsonl.zst", base, n)
	}
	if err := os.Rename(tmp, final); err != nil {
		return result, err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	result.File, result.Rows, result.Bytes = filepath.Base(final), len(ids), info.Size()
	for len(ids) > 0 {
		n := min(len(ids), 1000)
		if _, err := model.DeleteRelayPayloadsByIds(ctx, ids[:n]); err != nil {
			return result, err
		}
		ids = ids[n:]
	}
	return result, nil
}
