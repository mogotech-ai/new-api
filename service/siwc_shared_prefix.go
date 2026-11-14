package service

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// A cut is remembered by the whole lines before it, at least this long, so the
// first request of the next run (a new game) can be cut at the same place.
const siwcSharedCutMarkerBytes = 16

// Cuts are learned per message template, told apart by the first line: a
// heading another template repeats further down must not cut it there.
const siwcSharedTemplateBytes = 256

const siwcSharedCutsPerScope = 32

// Markers describe a caller's template rather than one run, and runs are hours
// apart (pregame reports for the day's games), so they outlive the texts.
const siwcSharedCutMarkerTTL = 24 * time.Hour

// A cut this close after an earlier request's cut moves back to it: the
// earlier one is already written upstream, so the request reads it now.
const siwcSharedSnapBytes = 8192

type siwcSharedText struct {
	scope [32]byte
	text  string
	cut   int // where this request was cut, so where it wrote the cache
	at    time.Time
}

type siwcSharedPrefixMemory struct {
	mu    sync.Mutex
	texts []siwcSharedText // oldest first
	bytes int
	cuts  map[[32]byte]map[string]time.Time // scope and first line -> text ending at an earlier cut -> last used
}

var siwcSharedPrefixes = newSIWCSharedPrefixMemory()

func newSIWCSharedPrefixMemory() *siwcSharedPrefixMemory {
	return &siwcSharedPrefixMemory{cuts: map[[32]byte]map[string]time.Time{}}
}

// foldSIWCSharedPrefix handles requests whose input is one long user message,
// such as a report asked once per market over the same game data. SIWC and
// Codex write the prompt cache only at the end of instructions and of the last
// user message, so the part siblings share is never cached. It moves the
// longest part shared with a recent request of the same caller, cut after a
// newline, to the end of instructions: the request writes the cache there
// itself and its siblings read it, with no extra upstream request. The first
// request of a run is cut where an earlier run was cut, when that text recurs.
func foldSIWCSharedPrefix(info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	setting := model_setting.GetGlobalSettings().SIWCSharedPrefixFold
	if !setting.Enabled || info.IsChannelTest || info.RelayMode != relayconstant.RelayModeResponses ||
		!slices.Contains(setting.TokenNames, info.TokenName) {
		return body, nil
	}
	request := gjson.ParseBytes(body)
	instructions, items := request.Get("instructions"), request.Get("input").Array()
	if instructions.Type != gjson.String || len(items) != 1 {
		return body, nil
	}
	item := items[0]
	if kind := item.Get("type").String(); (kind != "" && kind != "message") || item.Get("role").String() != "user" {
		return body, nil
	}
	content := item.Get("content")
	path := "input.0.content"
	if content.IsArray() {
		if len(content.Array()) != 1 {
			return body, nil
		}
		path = "input.0.content.0.text"
	}
	text, textOnly := siwcFoldableText(content, "")
	minBytes := setting.MinPrefixTokens * siwcPrefixBytesPerToken
	if !textOnly || len(instructions.String())+len(text) < minBytes {
		return body, nil
	}

	scope := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%s", info.TokenId, info.UserId, info.GetUpstreamModelName(), instructions.String())))
	cut := siwcSharedPrefixes.cut(scope, text, max(minBytes-len(instructions.String()), 1), time.Now(),
		time.Duration(setting.TTLSeconds)*time.Second, setting.MaxBytes)
	if cut == 0 {
		return body, nil
	}
	folded, err := sjson.SetBytes(body, "instructions", instructions.String()+"\n\n"+text[:cut])
	if err == nil {
		folded, err = sjson.SetBytes(folded, path, text[cut:])
	}
	if err != nil {
		return nil, err
	}
	logger.LogInfo(nil, common.LogText("SIWC shared prefix folded: token_id=%d channel_id=%d model=%q folded_bytes=%d kept_bytes=%d",
		info.TokenId, info.ChannelId, info.GetUpstreamModelName(), cut, len(text)-cut))
	return folded, nil
}

// cut remembers text and returns where to cut it, or 0. The cut is after the
// last newline inside the longest prefix shared with a remembered text of the
// scope, or after a cut marker of the same template when that reaches further;
// an earlier request's cut a little before it wins, because that one is cached.
// The cut leaves at least minCut bytes before it and something after it.
func (m *siwcSharedPrefixMemory) cut(scope [32]byte, text string, minCut int, now time.Time, ttl time.Duration, maxBytes int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.texts) > 0 && (now.Sub(m.texts[0].at) > ttl || m.bytes > maxBytes) {
		m.bytes -= len(m.texts[0].text)
		m.texts = m.texts[1:]
	}
	for key, markers := range m.cuts {
		for marker, at := range markers {
			if now.Sub(at) > max(ttl, siwcSharedCutMarkerTTL) {
				delete(markers, marker)
			}
		}
		if len(markers) == 0 {
			delete(m.cuts, key)
		}
	}

	valid := func(at int) bool { return at >= minCut && at < len(text) }
	shared := make([]int, len(m.texts))
	longest := 0
	for i, old := range m.texts {
		if old.scope == scope {
			shared[i] = siwcCommonPrefixLen(old.text, text)
			longest = max(longest, shared[i])
		}
	}
	cut := strings.LastIndexByte(text[:min(longest, len(text)-1)], '\n') + 1 // a repeated message keeps its last line
	if !valid(cut) {
		cut = 0
	}

	first := text[:min(len(text), siwcSharedTemplateBytes)]
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	template := sha256.Sum256(append(scope[:], first...))
	markers := m.cuts[template]
	if cut > 0 {
		start := cut // the marker is the whole lines before the cut
		for start > 0 && cut-start < siwcSharedCutMarkerBytes {
			start = strings.LastIndexByte(text[:start-1], '\n') + 1
		}
		if markers == nil {
			markers = map[string]time.Time{}
			m.cuts[template] = markers
		}
		if _, known := markers[text[start:cut]]; known || len(markers) < siwcSharedCutsPerScope {
			markers[text[start:cut]] = now
		}
	}
	for marker := range markers {
		if i := strings.LastIndex(text, marker); i >= 0 && i+len(marker) > cut && valid(i+len(marker)) {
			cut = i + len(marker)
			markers[marker] = now
		}
	}
	snapped := 0
	for i, old := range m.texts {
		if old.cut > 0 && old.cut <= cut && cut-old.cut < siwcSharedSnapBytes && shared[i] >= old.cut {
			snapped = max(snapped, old.cut)
		}
	}
	if snapped > 0 {
		cut = snapped
	}

	if len(text) <= maxBytes {
		m.texts = append(m.texts, siwcSharedText{scope: scope, text: text, cut: cut, at: now})
		m.bytes += len(text)
	}
	return cut
}

func siwcCommonPrefixLen(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i+64 <= n && a[i:i+64] == b[i:i+64] {
		i += 64
	}
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}
