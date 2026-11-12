package service

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// siwcPrefixBytesPerToken turns outgoing JSON bytes into estimated tokens.
// Forecast traffic measured ~3.1 text characters per token and JSON quoting
// only adds bytes, so 4 underestimates: thresholds lean towards fewer
// prewarms and the feedback check towards fewer false misses.
const siwcPrefixBytesPerToken = 4

const siwcPrefixAttemptWindow = 10 * time.Minute

// The per-scope rate map is swept of scopes idle for a minute once it holds
// this many; the global concurrency cap keeps live scopes far below it.
const siwcPrefixScopeSweepAt = 1024

// Top-level fields that do not change the upstream prompt tokens or cache
// routing. They stay out of the prefix scope, and the prewarm may override them.
var siwcPrefixScopeIgnoredFields = []string{"input", "stream", "stream_options", "max_output_tokens", "temperature", "top_p", "top_logprobs", "store", "metadata", "background"}

type siwcPrefixEntry struct {
	key        [32]byte
	seen       int       // requests whose input contained this boundary
	natural    bool      // it ended a real request, which wrote the cache there itself
	warmUntil  time.Time // the upstream cache write is assumed live until then
	warming    bool
	attempts   int
	attemptsAt time.Time // start of the current attempt window
}

type siwcPrefixIndex struct {
	mu       sync.Mutex
	entries  map[[32]byte]*list.Element
	lru      list.List // of *siwcPrefixEntry, most recently used first
	inflight int
	// Prewarm start times in the last minute per user/token/model/channel, oldest first.
	scopeRuns map[string][]time.Time
}

var siwcPrefixes = newSIWCPrefixIndex()

func newSIWCPrefixIndex() *siwcPrefixIndex {
	return &siwcPrefixIndex{entries: map[[32]byte]*list.Element{}, scopeRuns: map[string][]time.Time{}}
}

// entry returns the entry for key and marks it recently used; create adds a
// missing entry and evicts the least recently used ones beyond maxEntries.
func (idx *siwcPrefixIndex) entry(key [32]byte, create bool, maxEntries int) *siwcPrefixEntry {
	if element, ok := idx.entries[key]; ok {
		idx.lru.MoveToFront(element)
		return element.Value.(*siwcPrefixEntry)
	}
	if !create {
		return nil
	}
	element := idx.lru.PushFront(&siwcPrefixEntry{key: key})
	idx.entries[key] = element
	for idx.lru.Len() > max(maxEntries, 1) {
		oldest := idx.lru.Back()
		idx.lru.Remove(oldest)
		delete(idx.entries, oldest.Value.(*siwcPrefixEntry).key)
	}
	return element.Value.(*siwcPrefixEntry)
}

// SIWCPrefixPrewarm is a planned background request that makes upstream write
// the prompt cache at the end of a stable input prefix of a real request.
type SIWCPrefixPrewarm struct {
	key       [32]byte
	body      []byte
	tokenID   int
	channelID int
	model     string
	items     int
	estTokens int
}

// PlanSIWCPrefixPrewarm indexes the message boundaries of a final SIWC
// Responses body and returns a prewarm of the longest boundary that earlier
// requests repeated but upstream is not known to have cached, or nil. body is
// never changed. SIWC writes the prompt cache only at the end of instructions
// and of the last user message, and a request reuses the longest written
// prefix of itself, so history followed by changing messages is cached only
// if some request ended right there.
func PlanSIWCPrefixPrewarm(info *relaycommon.RelayInfo, body []byte) *SIWCPrefixPrewarm {
	if info == nil {
		return nil
	}
	info.SIWCWarmPrefixKey, info.SIWCWarmPrefixTokens = [32]byte{}, 0 // a retry may land on another channel
	setting := model_setting.GetGlobalSettings().SIWCPrefixPrewarm
	if !setting.Enabled || info.ChannelMeta == nil || !info.IsSIWC || info.IsChannelTest || info.RelayMode != relayconstant.RelayModeResponses {
		return nil
	}
	request := gjson.ParseBytes(body)
	input := request.Get("input")
	items := input.Array()
	users := make([]bool, len(items))
	lastUser := -1
	for i, item := range items {
		if kind := item.Get("type").String(); (kind == "" || kind == "message") && item.Get("role").String() == "user" {
			users[i], lastUser = true, i
		}
	}
	if lastUser < 0 {
		return nil
	}

	// The chain hash of item i covers every input to the upstream prompt and its
	// cache routing up to item i, so equal hashes are equal prefixes of the same
	// owner, and an edit anywhere in the history changes every later hash.
	scope := sha256.New()
	fmt.Fprintf(scope, "%d\x00%d\x00%x\x00%d\x00%d\x00%s\x00%s\x00", info.ChannelId, info.ChannelMultiKeyIndex,
		common.Sha256Raw([]byte(info.ApiKey)), info.UserId, info.TokenId, info.GetUpstreamModelName(), info.SIWCSessionID)
	request.ForEach(func(key, value gjson.Result) bool {
		if !slices.Contains(siwcPrefixScopeIgnoredFields, key.String()) {
			io.WriteString(scope, key.Raw)
			io.WriteString(scope, value.Raw)
		}
		return true
	})
	chain := scope.Sum(nil)
	hashes := make([][32]byte, lastUser+1)
	tokens := make([]int, lastUser+1)
	var boundaries []int // text-only user messages before the last user message, then the last user message
	size := len(body) - len(input.Raw)
	for i := range lastUser + 1 {
		next := sha256.New()
		next.Write(chain)
		io.WriteString(next, items[i].Raw)
		chain = next.Sum(chain[:0])
		hashes[i] = [32]byte(chain)
		size += len(items[i].Raw) + 1
		tokens[i] = size / siwcPrefixBytesPerToken
		if users[i] {
			if _, textOnly := siwcFoldableText(items[i].Get("content"), ""); textOnly || i == lastUser {
				boundaries = append(boundaries, i)
			}
		}
	}

	now := time.Now()
	ttl := time.Duration(setting.WarmTTLSeconds) * time.Second
	idx := siwcPrefixes
	idx.mu.Lock()
	defer idx.mu.Unlock()
	warm, target := -1, -1
	var warmEntry, targetEntry *siwcPrefixEntry
	for _, b := range boundaries {
		entry := idx.entry(hashes[b], false, setting.MaxEntries)
		if entry == nil {
			continue
		}
		if entry.warmUntil.After(now) {
			warm, warmEntry = b, entry
		}
		if b < lastUser && entry.seen >= setting.MinPriorSightings {
			target, targetEntry = b, entry
		}
	}
	warmTokens := 0
	if warmEntry != nil {
		// Upstream reads the longest written prefix, which keeps that write alive.
		warmEntry.warmUntil = now.Add(ttl)
		warmTokens = tokens[warm]
		if warm < lastUser { // a miss on the last user message is rewritten by this very request
			info.SIWCWarmPrefixKey, info.SIWCWarmPrefixTokens = hashes[warm], warmTokens
		}
	}

	// A boundary that ended an earlier request is history that later requests
	// extend and write past themselves (append-only chats resuming after the
	// TTL), so rewarming it would not be read.
	var prewarm *SIWCPrefixPrewarm
	if target > warm && !targetEntry.natural && !targetEntry.warming && tokens[target] >= setting.MinPrefixTokens && tokens[target]-warmTokens >= setting.MinGainTokens {
		if now.Sub(targetEntry.attemptsAt) >= siwcPrefixAttemptWindow {
			targetEntry.attempts, targetEntry.attemptsAt = 0, now
		}
		if len(idx.scopeRuns) >= siwcPrefixScopeSweepAt {
			for key, runs := range idx.scopeRuns {
				if now.Sub(runs[len(runs)-1]) >= time.Minute {
					delete(idx.scopeRuns, key)
				}
			}
		}
		scopeKey := fmt.Sprintf("%d:%d:%s:%d", info.UserId, info.TokenId, info.GetUpstreamModelName(), info.ChannelId)
		runs := slices.DeleteFunc(idx.scopeRuns[scopeKey], func(at time.Time) bool { return now.Sub(at) >= time.Minute })
		switch {
		case targetEntry.attempts >= setting.MaxAttempts || idx.inflight >= setting.MaxConcurrent:
		case len(runs) >= setting.MaxPerScopeMinute:
			logger.LogWarn(nil, common.LogText("SIWC prefix prewarm skipped: per-scope limit of %d per minute reached token_id=%d channel_id=%d model=%q",
				setting.MaxPerScopeMinute, info.TokenId, info.ChannelId, info.GetUpstreamModelName()))
		default:
			var truncated strings.Builder
			truncated.WriteByte('[')
			for i := range target + 1 {
				if i > 0 {
					truncated.WriteByte(',')
				}
				truncated.WriteString(items[i].Raw)
			}
			truncated.WriteByte(']')
			prewarmBody, err := sjson.SetRawBytes(body, "input", []byte(truncated.String()))
			if err == nil {
				prewarmBody, err = sjson.SetBytes(prewarmBody, "max_output_tokens", setting.MaxOutputTokens)
			}
			if err == nil {
				prewarmBody, err = sjson.SetBytes(prewarmBody, "stream", true)
			}
			if err != nil {
				logger.LogError(nil, common.LogText("SIWC prefix prewarm body failed: %s", err.Error()))
				break
			}
			targetEntry.warming = true
			targetEntry.attempts++
			idx.inflight++
			runs = append(runs, now)
			prewarm = &SIWCPrefixPrewarm{key: hashes[target], body: prewarmBody, tokenID: info.TokenId, channelID: info.ChannelId,
				model: info.GetUpstreamModelName(), items: target + 1, estTokens: tokens[target]}
		}
		if len(runs) == 0 {
			delete(idx.scopeRuns, scopeKey)
		} else {
			idx.scopeRuns[scopeKey] = runs
		}
	}

	for _, b := range boundaries {
		entry := idx.entry(hashes[b], true, setting.MaxEntries)
		entry.seen++
		if b == lastUser {
			entry.natural = true
			entry.warmUntil = now.Add(ttl) // the real request itself writes here
		}
	}
	return prewarm
}

// Start sends the prewarm in the background and returns at once. send must
// post body with the real request's URL, credential, headers and proxy; the
// stream is closed after the first output event, which already has upstream
// write the prompt cache. The prewarm is not billed to the caller.
func (p *SIWCPrefixPrewarm) Start(send func(ctx context.Context, body []byte) (*http.Response, error)) {
	common.RelayCtxGo(context.Background(), func() { p.run(send) })
}

// run sends the prewarm, releases its boundary and logs the outcome; the
// boundary is warm only once upstream started output.
func (p *SIWCPrefixPrewarm) run(send func(ctx context.Context, body []byte) (*http.Response, error)) {
	setting := model_setting.GetGlobalSettings().SIWCPrefixPrewarm
	started := time.Now()
	outcome, failure := "no_output", ""
	var usage gjson.Result
	defer func() {
		siwcPrefixes.mu.Lock()
		siwcPrefixes.inflight--
		if element, ok := siwcPrefixes.entries[p.key]; ok {
			entry := element.Value.(*siwcPrefixEntry)
			entry.warming = false
			if outcome == "warmed" {
				entry.warmUntil = time.Now().Add(time.Duration(setting.WarmTTLSeconds) * time.Second)
			}
		}
		siwcPrefixes.mu.Unlock()
		logger.LogInfo(nil, common.LogText("SIWC prefix prewarm: outcome=%s token_id=%d channel_id=%d model=%q items=%d est_tokens=%d input_tokens=%d cached_tokens=%d output_tokens=%d latency_ms=%d error=%q",
			outcome, p.tokenID, p.channelID, p.model, p.items, p.estTokens, usage.Get("input_tokens").Int(),
			usage.Get("input_tokens_details.cached_tokens").Int(), usage.Get("output_tokens").Int(), time.Since(started).Milliseconds(), failure))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(setting.TimeoutSeconds)*time.Second)
	defer cancel()
	resp, err := send(ctx, p.body)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) { // a base URL may carry a key in its query
			urlErr.URL = relaycommon.SanitizeURLForLog(urlErr.URL)
		}
		outcome, failure = "error", err.Error()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		outcome = fmt.Sprintf("http_%d", resp.StatusCode)
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 64<<20) // response.created echoes the instructions
	for outcome == "no_output" && scanner.Scan() {
		data, ok := bytes.CutPrefix(scanner.Bytes(), []byte("data:"))
		if !ok {
			continue
		}
		event := gjson.ParseBytes(data)
		switch eventType := event.Get("type").String(); eventType {
		case "", "response.created", "response.in_progress", "response.queued":
			continue
		case "response.failed", "response.error", "error":
			outcome = eventType
		default:
			outcome = "warmed"
		}
		usage = event.Get("response.usage")
	}
	if err := scanner.Err(); err != nil && outcome == "no_output" {
		failure = err.Error()
	}
}

// ObserveSIWCPrefixUsage marks the warm prefix a SIWC request expected to hit
// as cold when upstream reports under half of it cached, so the next request
// repeating it prewarms again within the attempt limit. Locally counted usage
// is not upstream evidence and is ignored.
func ObserveSIWCPrefixUsage(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage) {
	if info == nil || usage == nil || info.SIWCWarmPrefixTokens == 0 ||
		info.SIWCWarmPrefixTokens < model_setting.GetGlobalSettings().SIWCPrefixPrewarm.MinPrefixTokens {
		return
	}
	if usage.InputTokens == 0 || common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens) {
		return
	}
	cached := usage.PromptTokensDetails.CachedTokens
	if cached*2 >= info.SIWCWarmPrefixTokens {
		return
	}
	siwcPrefixes.mu.Lock()
	if element, ok := siwcPrefixes.entries[info.SIWCWarmPrefixKey]; ok {
		element.Value.(*siwcPrefixEntry).warmUntil = time.Time{}
	}
	siwcPrefixes.mu.Unlock()
	logger.LogInfo(c, common.LogText("SIWC prefix cache miss: expected_tokens=%d cached_tokens=%d token_id=%d channel_id=%d model=%q",
		info.SIWCWarmPrefixTokens, cached, info.TokenId, info.ChannelId, info.GetUpstreamModelName()))
}
