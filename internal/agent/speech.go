package agent

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 語音通知（設定 voiceNotifications；C# 的 Speech/VoiceAnnouncer.cs、Myriagent.Mac/Speech/MacSpeechEngine.cs、AgentHub.Events.cs 的 AnnounceAttention）：
// 一條背景 goroutine 依序念佇列裡的句子，一句念完才念下一句（佇列上限與重複的規則見 voiceShouldQueue）。
// 聲音引擎在第一次要念時才建立；建立失敗（macOS 沒有 /usr/bin/say、Linux 沒有 spd-say）記一次警告，之後都不念。
// 每句帶自己的語速與音調，也可以帶「開始念了」的回呼（聲音出來的那一刻；網頁同時讓那個對話的標頭閃三下）。
// stop 清掉佇列並停下正在念的那句（關掉設定、調整語速時）。

// speechEngine：念一句話的聲音引擎（只在念的 goroutine 上用）
type speechEngine interface {
	// Speak 念 text：念完或 shouldStop 回 true 才返回；聲音開始出來時呼叫 onStart（沒念出來就不呼叫）
	Speak(text string, tuning voiceTuning, shouldStop func() bool, onStart func()) error
}

type voiceReq struct {
	text    string
	tuning  voiceTuning
	started func()
}

type voiceAnnouncer struct {
	create func() (speechEngine, error)
	now    func() time.Time

	mu          sync.Mutex
	cond        *sync.Cond
	pending     []voiceReq
	lastText    string
	lastAt      time.Time
	worker      bool
	speaking    bool
	generation  int64 // stop 時加一：正在念的那句開始時記下的值不同了就停下
	unavailable bool
	disposed    bool
}

func newVoiceAnnouncer(create func() (speechEngine, error)) *voiceAnnouncer {
	a := &voiceAnnouncer{create: create, now: time.Now}
	a.cond = sync.NewCond(&a.mu)
	return a
}

// speak 排進佇列（佇列滿了、或同一句剛念過就略過）
func (a *voiceAnnouncer) speak(text string, tuning voiceTuning, started func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.disposed || a.unavailable {
		return
	}
	now := a.now()
	if !voiceShouldQueue(text, len(a.pending), a.lastText, a.lastAt, now) {
		return
	}
	a.lastText, a.lastAt = text, now
	a.pending = append(a.pending, voiceReq{text, tuning, started})
	if !a.worker {
		a.worker = true
		go a.run()
	} else {
		a.cond.Broadcast()
	}
}

// stop 停下正在念的那句、清掉還沒念的
func (a *voiceAnnouncer) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = nil
	a.lastText, a.lastAt = "", time.Time{}
	a.generation++
	a.cond.Broadcast()
}

func (a *voiceAnnouncer) idle() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending) == 0 && !a.speaking
}

func (a *voiceAnnouncer) dispose() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.disposed = true
	a.pending = nil
	a.generation++
	a.cond.Broadcast()
}

func (a *voiceAnnouncer) run() {
	var engine speechEngine
	for {
		a.mu.Lock()
		for len(a.pending) == 0 && !a.disposed {
			a.cond.Wait()
		}
		if a.disposed {
			a.mu.Unlock()
			return
		}
		req := a.pending[0]
		a.pending = a.pending[1:]
		gen := a.generation
		a.speaking = true
		a.mu.Unlock()

		if engine == nil {
			e, err := a.create()
			if err != nil {
				log.Printf("語音通知無法使用：%v", err)
				a.mu.Lock()
				a.unavailable = true
				a.pending = nil
				a.speaking = false
				a.worker = false
				a.mu.Unlock()
				return
			}
			engine = e
		}
		err := engine.Speak(req.text, req.tuning, func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.disposed || a.generation != gen
		}, func() {
			if req.started != nil {
				req.started()
			}
		})
		if err != nil {
			log.Printf("語音通知念不出來：%v", err)
		}
		a.mu.Lock()
		a.speaking = false
		a.mu.Unlock()
	}
}

// ---- 聲音引擎 ----

// newSpeechEngine：這台的聲音引擎（測試換成假的）
var newSpeechEngine = defaultSpeechEngine

func defaultSpeechEngine() (speechEngine, error) {
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/usr/bin/say"); err != nil {
			return nil, errors.New("這台沒有系統的語音工具（/usr/bin/say）")
		}
		return &sayEngine{voice: findSayVoice()}, nil
	}
	p, err := exec.LookPath("spd-say")
	if err != nil {
		return nil, errors.New("這台沒有 spd-say（speech-dispatcher）；裝了之後重新打開語音通知")
	}
	return &spdEngine{path: p}, nil
}

// sayEngine：macOS 內建的 say。語速以每分鐘字數給（倍速 × 180），音調以內嵌的指令升降（[[pbas ±n]]，單位是半音）；
// 文字從標準輸入給（不受命令列長度限制，也不會被當成選項），代理的文字可能有「[[」，先拆掉才不會被當成指令
type sayEngine struct{ voice string }

const sayBaseWordsPerMinute = 180

// 優先用的聲音（都沒有裝就找任何一個台灣中文的，再沒有就用系統設定的）
var sayPreferredVoices = []string{"Meijia", "Mei-Jia", "美佳"}

func findSayVoice() string {
	out, err := exec.Command("/usr/bin/say", "-v", "?").Output()
	if err != nil {
		log.Printf("列不出這台的語音：%v", err)
		return ""
	}
	return pickSayVoice(string(out))
}

// pickSayVoice：從 say -v ? 的輸出（每行以聲音的名稱開頭）挑第一個偏好的；沒有就挑台灣中文的
func pickSayVoice(listing string) string {
	lines := strings.Split(listing, "\n")
	for _, name := range sayPreferredVoices {
		for _, l := range lines {
			if strings.HasPrefix(strings.ToLower(l), strings.ToLower(name)+" ") {
				return name
			}
		}
	}
	for _, l := range lines {
		if strings.Contains(l, " zh_TW ") {
			if i := strings.Index(l, "  "); i > 0 {
				return strings.TrimSpace(l[:i])
			}
		}
	}
	return ""
}

// sayArgs、sayInput：say 的參數與要寫進標準輸入的文字（純函式，方便測）
func sayArgs(voice string, t voiceTuning) []string {
	args := []string{}
	if voice != "" {
		args = append(args, "-v", voice)
	}
	speed := math.Max(0.5, math.Min(3, t.Speed))
	return append(args, "-r", strconv.Itoa(int(math.Round(sayBaseWordsPerMinute*speed))), "-f", "-")
}

func sayInput(text string, t voiceTuning) string {
	semitones := int(math.Round(t.Pitch))
	pitch := ""
	if semitones > 0 {
		pitch = fmt.Sprintf("[[pbas +%d]] ", semitones)
	} else if semitones < 0 {
		pitch = fmt.Sprintf("[[pbas -%d]] ", -semitones)
	}
	return pitch + strings.ReplaceAll(text, "[[", "[ [")
}

func (e *sayEngine) Speak(text string, t voiceTuning, shouldStop func() bool, onStart func()) error {
	cmd := exec.Command("/usr/bin/say", sayArgs(e.voice, t)...)
	cmd.Stdin = strings.NewReader(sayInput(text, t))
	return runSpeech(cmd, shouldStop, onStart, nil)
}

// spdEngine：Linux 的 spd-say（speech-dispatcher）。語速、音調是 -100～100：倍速 1＝0、每多 0.01 倍加 1；每個半音約 100/6。
// -w 等念完才結束；-e 從標準輸入讀文字。停下時另外叫 spd-say -S（殺掉用戶端不會讓 speech-dispatcher 停下）
type spdEngine struct{ path string }

func spdArgs(t voiceTuning) []string {
	rate := int(math.Round(math.Max(-100, math.Min(100, (t.Speed-1)*100))))
	pitch := int(math.Round(math.Max(-100, math.Min(100, t.Pitch*100/6))))
	return []string{"-w", "-e", "-r", strconv.Itoa(rate), "-p", strconv.Itoa(pitch)}
}

func (e *spdEngine) Speak(text string, t voiceTuning, shouldStop func() bool, onStart func()) error {
	cmd := exec.Command(e.path, spdArgs(t)...)
	cmd.Stdin = strings.NewReader(text)
	return runSpeech(cmd, shouldStop, onStart, func() { _ = exec.Command(e.path, "-S").Run() })
}

// runSpeech：啟動、呼叫 onStart、等它結束；shouldStop 回 true 時殺掉（再呼叫 cancel）
func runSpeech(cmd *exec.Cmd, shouldStop func() bool, onStart func(), cancel func()) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	onStart()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-tick.C:
			if shouldStop() {
				_ = cmd.Process.Kill()
				if cancel != nil {
					cancel()
				}
				<-done
				return nil
			}
		}
	}
}

// ---- 中樞 ----

// 各中樞的語音通知（第一次用到時建立）
var hubVoices sync.Map // *Hub → *voiceAnnouncer

func (h *Hub) voice() *voiceAnnouncer {
	if a, ok := hubVoices.Load(h); ok {
		return a.(*voiceAnnouncer)
	}
	a, _ := hubVoices.LoadOrStore(h, newVoiceAnnouncer(func() (speechEngine, error) { return newSpeechEngine() }))
	return a.(*voiceAnnouncer)
}

// voiceTuningSetting：設定的語速與音調（正規化過；設定檔被手改成範圍外的值也照範圍念）。呼叫時持有 h.mu
func (h *Hub) voiceTuningSetting() voiceTuning {
	return voiceTuning{normalizeVoiceSpeed(settingNumber(h.setting("voiceSpeed"), voiceDefaultSpeed)), normalizeVoicePitch(settingNumber(h.setting("voicePitch"), voiceDefaultPitch))}
}

func settingNumber(v any, def float64) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return def
}

// speak：以設定的語速與音調念一句（不看 voiceNotifications；呼叫時持有 h.mu）。排程失敗之類的通知也用它
func (h *Hub) speak(text string, started func()) {
	h.voice().speak(text, h.voiceTuningSetting(), started)
}

// flashConversation：要所有視窗閃這個對話（web-protocol 的 flash）：speaking 是 false＝開始一直閃到使用者點它，
// true＝語音通知開始念它（還在閃的標頭另外閃三下）。呼叫時持有 h.mu
func (h *Hub) flashConversation(convID string, speaking bool) {
	m := map[string]any{"type": "flash", "conversationId": convID}
	if speaking {
		m["speaking"] = true
	}
	h.broadcast(m)
}

// announceAttention：要你回來看的事（完成、失敗、提問、核准、計畫）：馬上閃；語音通知開著時再念這句，
// 聲音開始出來時另送帶 speaking 的 flash。呼叫時持有 h.mu
func (h *Hub) announceAttention(c *Conv, text string) {
	if c.Meta.IsDraft && len(c.Items) == 0 {
		return
	}
	id := c.Meta.ID
	h.flashConversation(id, false)
	if h.settingBool("voiceNotifications") && strings.TrimSpace(text) != "" {
		h.speak(text, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.flashConversation(id, true)
		})
	}
}

// onTurnEndedAttention：一輪結束（endTurn 在清掉狀態之前呼叫；持有 h.mu）。使用者按停止的那一輪不念；
// 這一輪有錯誤項目＝失敗（念錯誤的第一行，中文才念）；否則完成（接代理最後一則回覆的重點）
func (h *Hub) onTurnEndedAttention(c *Conv) {
	if c.stopping {
		return
	}
	for i := len(c.Items) - 1; i >= 0; i-- {
		it := c.Items[i]
		if it.Kind == "User" {
			break
		}
		if it.Kind == "Error" {
			h.announceAttention(c, voiceFailed(c.Meta.Title, it.Text))
			return
		}
	}
	h.announceAttention(c, voiceCompleted(c.Meta.Title, lastReplyText(c)))
}

// lastReplyText：這一輪代理最後一則有文字的回覆（從尾端往前找，碰到使用者的訊息就停）
func lastReplyText(c *Conv) string {
	for i := len(c.Items) - 1; i >= 0; i-- {
		it := c.Items[i]
		if it.Kind == "User" {
			return ""
		}
		if it.Kind == "Assistant" && strings.TrimSpace(it.Text) != "" {
			return it.Text
		}
	}
	return ""
}

// requestAttention：代理提問、要求核准、計畫等確認（各家建好等待中的卡片後呼叫；持有 h.mu）。plan 是計畫的原文（PlanApproval 才用）
func (h *Hub) requestAttention(c *Conv, it *Item, plan string) {
	switch it.Kind {
	case "Question":
		q := ""
		if len(it.Questions) > 0 {
			q = it.Questions[0].Text
		}
		h.announceAttention(c, voiceQuestion(c.Meta.Title, q))
	case "PlanApproval":
		h.announceAttention(c, voicePlan(c.Meta.Title, plan))
	default:
		h.announceAttention(c, voiceApproval(c.Meta.Title, it.ToolName, it.Detail, it.FilePath))
	}
}

func init() {
	// 打開時念一句試聽（使用者馬上知道聲音與音量）；關掉時停下正在念的
	registerSettingHook("voiceNotifications", func(h *Hub, before, value any) any {
		on, _ := value.(bool)
		was, _ := before.(bool)
		if on && !was {
			h.voice().speak(voiceEnabledSample, h.voiceTuningSetting(), nil)
		} else if !on {
			h.voice().stop()
		}
		return value
	})
	// 語速、音調：限制在範圍內並取到間距；開著語音通知時先停下正在念的，再以新的設定念一句試聽（連續調好幾次只聽到最後一次）
	tune := func(key string) func(h *Hub, before, value any) any {
		return func(h *Hub, before, value any) any {
			x, _ := value.(float64)
			old := h.voiceTuningSetting()
			var v float64
			if key == "voiceSpeed" {
				v = normalizeVoiceSpeed(x)
			} else {
				v = normalizeVoicePitch(x)
			}
			nt := old
			if key == "voiceSpeed" {
				nt.Speed = v
			} else {
				nt.Pitch = v
			}
			if h.settingBool("voiceNotifications") && nt != old {
				h.voice().stop()
				h.voice().speak(voiceTuningSample, nt, nil)
			}
			return v
		}
	}
	registerSettingHook("voiceSpeed", tune("voiceSpeed"))
	registerSettingHook("voicePitch", tune("voicePitch"))
}
