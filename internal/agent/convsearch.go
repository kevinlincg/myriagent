package agent

import (
	"context"
	"strings"
	"sync"
	"unicode"
)

// 側欄的對話搜尋（標題與內文；web-protocol 的 searchConversations／conversationSearch）。規則照 Protocol 的 ConversationSearchRules：
// 查詢以空白分成關鍵字，每個關鍵字都要出現在標題或內文（不分大小寫，同網頁 searchrules.js 的標題比對）；
// 內文只看使用者訊息、代理回覆與計畫確認的計畫內容（工具輸出、思考與提示不算：太雜，搜出來的對話多半不是要找的）。
// Go 版的對話一律整個在記憶體（loadConversations 就讀了 items.json），所以不必像 C# 那樣另讀檔、快取：
// 在鎖裡擷取各對話的標題與內文，到背景比對；同一個視窗的新搜尋取消上一個（被取消的不回覆），回覆只給發出的視窗。

const (
	searchMaxQuery      = 200
	searchMaxTerms      = 8
	searchSnippetBefore = 12
	searchSnippetAfter  = 60
	searchSnippetWindow = 400
)

// searchTerms：查詢 → 關鍵字（以空白分開、不分大小寫去重複、保留第一次的寫法與順序、最多 8 個；整串先截到 200 字）
func searchTerms(query string) []string {
	r := []rune(query)
	if len(r) > searchMaxQuery {
		r = r[:searchMaxQuery]
	}
	terms := []string{}
	for _, part := range strings.Fields(string(r)) {
		dup := false
		for _, t := range terms {
			if strings.EqualFold(t, part) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		terms = append(terms, part)
		if len(terms) == searchMaxTerms {
			break
		}
	}
	return terms
}

func isSearchableKind(kind string) bool {
	return kind == "User" || kind == "Assistant" || kind == "PlanApproval"
}

func containsFold(s, sub string) bool { return indexOfFold([]rune(s), []rune(sub)) >= 0 }

// searchMatch：每個關鍵字都出現在標題或任一段內文才符合（ok）；snippet 取依序第一段含任一關鍵字的內文、從最早的符合處截，
// 只有標題符合時是 nil。沒有關鍵字回不符合
func searchMatch(title string, texts []string, terms []string) (snippet *string, ok bool) {
	if len(terms) == 0 {
		return nil, false
	}
	remaining := []string{}
	for _, t := range terms {
		if title == "" || !containsFold(title, t) {
			remaining = append(remaining, t)
		}
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		if snippet == nil {
			rt := []rune(text)
			first, firstLen := -1, 0
			for _, t := range terms {
				at := indexOfFold(rt, []rune(t))
				if at >= 0 && (first < 0 || at < first) {
					first, firstLen = at, len([]rune(t))
				}
			}
			if first >= 0 {
				s := searchSnippet(rt, first, firstLen)
				snippet = &s
			}
		}
		if len(remaining) > 0 {
			kept := remaining[:0]
			for _, t := range remaining {
				if !containsFold(text, t) {
					kept = append(kept, t)
				}
			}
			remaining = kept
		}
		if len(remaining) == 0 && snippet != nil {
			break
		}
	}
	if len(remaining) != 0 {
		return nil, false
	}
	return snippet, true
}

// searchSnippet：符合處（index 起 length 字）前後的一行片段：連續的空白與換行併成一個空格，前面最多留 12 字、從符合處起最多 60 字
// （關鍵字本身比這個長時整個留著），有截掉內容的那一端加「…」
func searchSnippet(text []rune, index, length int) string {
	index = clampInt(index, 0, len(text))
	length = clampInt(length, 0, len(text)-index)

	beforeStart := max(0, index-searchSnippetWindow)
	before := []rune(strings.TrimLeftFunc(collapseSpaces(text[beforeStart:index]), unicode.IsSpace))
	cutStart := len(before) > searchSnippetBefore || hasContent(text[:beforeStart])
	if len(before) > searchSnippetBefore {
		before = []rune(strings.TrimLeftFunc(string(before[len(before)-searchSnippetBefore:]), unicode.IsSpace))
	}

	limit := max(searchSnippetAfter, length)
	afterEnd := min(len(text), index+limit+searchSnippetWindow)
	after := []rune(strings.TrimRightFunc(collapseSpaces(text[index:afterEnd]), unicode.IsSpace))
	cutEnd := len(after) > limit || hasContent(text[afterEnd:])
	if len(after) > limit {
		after = []rune(strings.TrimRightFunc(string(after[:limit]), unicode.IsSpace))
	}
	out := string(before) + string(after)
	if cutStart {
		out = "…" + out
	}
	if cutEnd {
		out += "…"
	}
	return out
}

func clampInt(v, lo, hi int) int { return max(lo, min(v, hi)) }

// collapseSpaces：連續的空白（含換行、全形空白）併成一個空格
func collapseSpaces(rs []rune) string {
	var sb strings.Builder
	space := false
	for _, r := range rs {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space {
			sb.WriteByte(' ')
		}
		space = false
		sb.WriteRune(r)
	}
	if space {
		sb.WriteByte(' ')
	}
	return sb.String()
}

func hasContent(rs []rune) bool {
	for _, r := range rs {
		if !unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// ---- 中樞 ----

type searchSource struct {
	id, title string
	texts     []string
}

// searchRuns：各視窗進行中的搜尋（新的取消舊的；searchMu 保護）
var (
	searchMu   sync.Mutex
	searchRuns = map[*View]context.CancelFunc{}
)

func init() {
	registerCommand("searchConversations", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		h.searchConversations(v, msg["requestId"], str(msg, "query"))
	})
}

// searchConversations（呼叫時持有 h.mu）
func (h *Hub) searchConversations(v *View, requestID any, query string) {
	searchMu.Lock()
	if cancel := searchRuns[v]; cancel != nil {
		cancel()
		delete(searchRuns, v)
	}
	searchMu.Unlock()
	terms := searchTerms(query)
	if len(terms) == 0 {
		h.sendTo(v, map[string]any{"type": "conversationSearch", "requestId": requestID, "query": query, "hits": []any{}})
		return
	}
	// 範圍同對話清單（不含草稿、最多 1000 個、依清單順序）
	var sources []searchSource
	for _, s := range h.conversationList() {
		c := h.convs[s.ID]
		if c == nil {
			continue
		}
		src := searchSource{id: c.Meta.ID, title: c.Meta.Title}
		for _, it := range c.Items {
			if isSearchableKind(it.Kind) && it.Text != "" {
				src.texts = append(src.texts, it.Text)
			}
		}
		sources = append(sources, src)
	}
	ctx, cancel := context.WithCancel(context.Background())
	searchMu.Lock()
	searchRuns[v] = cancel
	searchMu.Unlock()
	go func() {
		hits := []map[string]any{}
		for _, s := range sources {
			if ctx.Err() != nil {
				return
			}
			if snippet, ok := searchMatch(s.title, s.texts, terms); ok {
				var sn any
				if snippet != nil {
					sn = *snippet
				}
				hits = append(hits, map[string]any{"conversationId": s.id, "snippet": sn})
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		// 已被新的搜尋取代：不送
		if ctx.Err() != nil {
			return
		}
		searchMu.Lock()
		if ctx.Err() == nil {
			delete(searchRuns, v)
		}
		searchMu.Unlock()
		cancel()
		h.sendTo(v, map[string]any{"type": "conversationSearch", "requestId": requestID, "query": query, "hits": hits})
	}()
}
