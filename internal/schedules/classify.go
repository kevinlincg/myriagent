package schedules

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Origin 排程的來源分類（排程頁的篩選、要不要列出、要不要自動診斷）。Agent：有證據是代理建立或改過的；Adopted：使用者按了「納入管理」；
// Suspected：沒有證據但看起來是自己開發的；Other：系統、廠商、更新程式；Ignored：使用者按了「不再列出」
type Origin int

const (
	OriginAgent Origin = iota
	OriginAdopted
	OriginSuspected
	OriginOther
	OriginIgnored
)

// Name 網頁用的名稱
func (o Origin) Name() string {
	switch o {
	case OriginAgent:
		return "agent"
	case OriginAdopted:
		return "adopted"
	case OriginSuspected:
		return "suspected"
	case OriginIgnored:
		return "ignored"
	}
	return "other"
}

// Environment 判斷用的這台電腦的資料夾（展開變數、比對廠商位置）。WorkRoots＝雖然在廠商位置底下、但是使用者工作的資料夾
// （Myriagent 對話自己的工作區在資料根目錄底下，代理在那裡寫的腳本不是廠商的）。路徑是 / 分隔、變數是 ~ 與 $HOME
type Environment struct {
	UserName    string
	MachineName string
	Variables   map[string]string
	WorkRoots   []string
}

// VendorRoots 廠商與系統的位置（小寫、結尾有 /）：/Applications、/Library、/System、/usr、/opt、/bin、/sbin、/private、/var、/etc，
// 以及使用者的 ~/Library、~/Applications
func (e Environment) VendorRoots() []string {
	roots := []string{"/applications/", "/library/", "/system/", "/usr/", "/opt/", "/bin/", "/sbin/", "/private/", "/var/", "/etc/"}
	if home := e.Variables["HOME"]; strings.TrimSpace(home) != "" {
		h := strings.ToLower(strings.TrimRight(home, "/"))
		roots = append(roots, h+"/library/", h+"/applications/")
	}
	return roots
}

// ClassifyInput 判斷「疑似代理建立」的輸入（路徑在不在 git repo 要讀檔案系統，這裡只吃結果）
type ClassifyInput struct {
	Path          string
	Author        string
	Actions       []Action
	AnyPathInRepo bool
	Env           Environment
}

var interpreters = toSet("python", "pythonw", "py", "pyw", "node", "powershell", "pwsh", "cmd", "bash", "sh", "wsl", "wscript", "cscript", "dotnet",
	"deno", "bun", "uv", "uvx", "npx", "npm", "php", "ruby", "perl", "java", "javaw", "rscript",
	// macOS、Linux 常見的
	"zsh", "dash", "ksh", "fish", "python3", "osascript", "env")

var scriptExtensions = toSet(".ps1", ".py", ".pyw", ".bat", ".cmd", ".js", ".mjs", ".cjs", ".ts", ".sh", ".vbs", ".rb", ".pl", ".php", ".zsh", ".command")

func toSet(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range items {
		m[strings.ToLower(s)] = true
	}
	return m
}

// posixPath 指令裡夾著的絕對路徑（sh -c 的字串、>> 轉向的檔案）：~、$HOME 或 / 開頭，到空白或殼層的符號為止。
// Go 的 regexp 沒有往回看，前一個字另外檢查（pathBoundaryOK）
var posixPath = regexp.MustCompile(`(?:~|\$HOME|\$\{HOME\})?/[\w.@%+=:,-]+(?:/[\w.@%+=:,-]+)*`)

var posixVar = regexp.MustCompile(`\$(?:\{(\w+)\}|(\w+))`)

func pathBoundaryOK(s string, idx int) bool {
	if idx == 0 {
		return true
	}
	c := s[idx-1]
	return !(c == '_' || c == '.' || c == '$' || c == '~' || c == '/' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80)
}

// ClassifyOrigin 沒有證據時的分類：Suspected 或 Other
func ClassifyOrigin(in ClassifyInput) Origin {
	var execs []Action
	for _, a := range in.Actions {
		if a.Kind == "Exec" && strings.TrimSpace(a.Command) != "" {
			execs = append(execs, a)
		}
	}
	if len(execs) == 0 {
		return OriginOther
	}
	if in.AnyPathInRepo {
		return OriginSuspected
	}
	if IsVendorAuthor(in.Author, in.Env) {
		return OriginOther
	}
	for _, a := range execs {
		command, ok := Expand(a.Command, in.Env)
		if !ok {
			continue
		}
		name := fileStem(command)
		isScript := scriptExtensions[strings.ToLower(extension(command))]
		paths := PathsIn(a.Arguments, in.Env)
		if a.WorkingDirectory != "" {
			if wd, ok := Expand(a.WorkingDirectory, in.Env); ok {
				paths = append(paths, wd)
			}
		}
		if interpreters[strings.ToLower(name)] {
			// 直譯器：參數或工作資料夾有一個不在廠商位置就算；完全沒有路徑（例如 sh -c 一行指令）時作者是這個使用者才算
			if len(paths) == 0 {
				if IsUserAuthor(in.Author, in.Env) {
					return OriginSuspected
				}
			} else {
				for _, p := range paths {
					if !IsVendorPath(p, in.Env) {
						return OriginSuspected
					}
				}
			}
			continue
		}
		if isScript || isAbsolute(command) {
			if !IsVendorPath(command, in.Env) {
				return OriginSuspected
			}
		}
	}
	return OriginOther
}

// CandidatePaths 要檢查在不在 git repo 的路徑：指令本身、參數裡的絕對路徑、工作資料夾
func CandidatePaths(actions []Action, env Environment) []string {
	var list []string
	for _, a := range actions {
		if a.Kind != "Exec" {
			continue
		}
		if c, ok := Expand(a.Command, env); ok && a.Command != "" && isAbsolute(c) {
			list = append(list, c)
		}
		list = append(list, PathsIn(a.Arguments, env)...)
		if wd, ok := Expand(a.WorkingDirectory, env); ok && a.WorkingDirectory != "" && isAbsolute(wd) {
			list = append(list, wd)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range list {
		k := strings.ToLower(p)
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// WorkFolder 排程的「工作資料夾」：動作的工作資料夾，沒有就是參數裡第一個腳本或指令所在的資料夾
func WorkFolder(actions []Action, env Environment) string {
	for _, a := range actions {
		if a.Kind == "Exec" && a.WorkingDirectory != "" {
			if wd, ok := Expand(a.WorkingDirectory, env); ok && isAbsolute(wd) {
				if len(wd) > 1 {
					return strings.TrimRight(wd, "/")
				}
				return wd
			}
		}
	}
	for _, a := range actions {
		if a.Kind != "Exec" {
			continue
		}
		for _, p := range PathsIn(a.Arguments, env) {
			if !IsVendorPath(p, env) {
				return directoryOf(p)
			}
		}
		if c, ok := Expand(a.Command, env); ok && a.Command != "" && isAbsolute(c) && !IsVendorPath(c, env) {
			return directoryOf(c)
		}
	}
	return ""
}

// ActionText 一行顯示的動作：指令＋參數（多個動作以「；」接起來）
func ActionText(actions []Action) string {
	var parts []string
	for _, a := range actions {
		if a.Kind == "Exec" {
			s := quoteCommand(a.Command)
			if strings.TrimSpace(a.Arguments) != "" {
				s += " " + a.Arguments
			}
			parts = append(parts, strings.TrimSpace(s))
			continue
		}
		switch a.Kind {
		case "ComHandler":
			parts = append(parts, "COM 元件")
		case "SendEmail":
			parts = append(parts, "寄電子郵件")
		case "ShowMessage":
			parts = append(parts, "顯示訊息")
		default:
			parts = append(parts, a.Kind)
		}
	}
	return strings.Join(parts, "；")
}

func quoteCommand(c string) string {
	if strings.Contains(c, " ") && !strings.HasPrefix(c, `"`) {
		return `"` + c + `"`
	}
	return c
}

// PathsIn 參數裡的絕對路徑：整個參數就是路徑的（可以有空白，引號包著）照整個算；夾在別的字裡的（sh -c 的字串、轉向）以樣式找。
// ~ 與 $HOME 換成使用者資料夾；/dev/ 底下的（/dev/null）不算
func PathsIn(arguments string, env Environment) []string {
	if strings.TrimSpace(arguments) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, token := range PosixTokens(arguments) {
		if whole, ok := Expand(token, env); ok && isAbsolute(whole) && !strings.ContainsAny(whole, "|;&<>$`") {
			if !strings.HasPrefix(whole, "/dev/") && !seen[whole] {
				seen[whole] = true
				if len(whole) > 1 {
					whole = strings.TrimRight(whole, "/")
				}
				out = append(out, whole)
			}
			continue
		}
		for _, loc := range posixPath.FindAllStringIndex(token, -1) {
			if !pathBoundaryOK(token, loc[0]) {
				continue
			}
			if p, ok := Expand(token[loc[0]:loc[1]], env); ok && len(p) > 1 && !strings.HasPrefix(p, "/dev/") && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// PosixTokens 以空白切成參數（單引號、雙引號裡的不切，引號拿掉；反斜線跳脫下一個字）
func PosixTokens(text string) []string {
	var tokens []string
	var sb strings.Builder
	var quote rune
	any := false
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		ch := rs[i]
		if quote != 0 {
			switch {
			case ch == quote:
				quote = 0
			case ch == '\\' && quote == '"' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\'):
				i++
				sb.WriteRune(rs[i])
			default:
				sb.WriteRune(ch)
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			any = true
			continue
		}
		if ch == '\\' && i+1 < len(rs) {
			i++
			sb.WriteRune(rs[i])
			any = true
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			if sb.Len() > 0 || any {
				tokens = append(tokens, sb.String())
			}
			sb.Reset()
			any = false
			continue
		}
		sb.WriteRune(ch)
		any = true
	}
	if sb.Len() > 0 || any {
		tokens = append(tokens, sb.String())
	}
	return tokens
}

// Expand 展開 ~、$變數；有展不開的變數回 false（不知道在哪）
func Expand(path string, env Environment) (string, bool) {
	p := strings.Trim(strings.TrimSpace(path), `"'`)
	home := env.Variables["HOME"]
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home == "" {
			return "", false
		}
		p = strings.TrimRight(home, "/") + p[1:]
	}
	unknown := false
	out := posixVar.ReplaceAllStringFunc(p, func(m string) string {
		sub := posixVar.FindStringSubmatch(m)
		name := sub[1]
		if name == "" {
			name = sub[2]
		}
		if v, ok := env.Variables[name]; ok {
			return v
		}
		unknown = true
		return m
	})
	if unknown {
		return "", false
	}
	return out, true
}

// IsVendorPath 在廠商或系統的位置（工作資料夾底下的不算）
func IsVendorPath(path string, env Environment) bool {
	p := strings.ToLower(path)
	for _, w := range env.WorkRoots {
		if strings.HasPrefix(p, strings.ToLower(strings.TrimRight(w, "/"))+"/") {
			return false
		}
	}
	for _, r := range env.VendorRoots() {
		if strings.HasPrefix(p, r) || p+"/" == r {
			return true
		}
	}
	return false
}

// IsVendorAuthor 作者是公司或程式名稱（不是這個使用者、不是「電腦\使用者」、不是 SID）
func IsVendorAuthor(author string, env Environment) bool {
	if strings.TrimSpace(author) == "" {
		return false
	}
	return !IsUserAuthor(author, env) && !strings.Contains(author, `\`) && !strings.HasPrefix(strings.ToUpper(author), "S-1-")
}

// IsUserAuthor 作者是這個使用者（沒寫也算）
func IsUserAuthor(author string, env Environment) bool {
	if strings.TrimSpace(author) == "" {
		return true
	}
	a := strings.TrimSpace(author)
	return strings.EqualFold(a, env.UserName) || strings.EqualFold(a, env.MachineName+`\`+env.UserName) ||
		strings.HasSuffix(strings.ToLower(a), strings.ToLower(`\`+env.UserName))
}

func isAbsolute(p string) bool { return len(p) > 1 && p[0] == '/' }

func fileStem(command string) string {
	name := command[strings.LastIndexAny(command, `\/`)+1:]
	if dot := strings.LastIndex(name, "."); dot > 0 {
		return name[:dot]
	}
	return name
}

func extension(command string) string {
	slash := strings.LastIndexAny(command, `\/`)
	dot := strings.LastIndex(command, ".")
	if dot > slash {
		return command[dot:]
	}
	return ""
}

func directoryOf(p string) string {
	if last := strings.LastIndex(p, "/"); last > 0 {
		return p[:last]
	}
	return p
}

// RepoRootOf 往上找 .git（資料夾或 worktree 的檔案）；外接與網路磁碟（/Volumes、/net、/mnt、/media）不碰（檢查存在可能卡在斷線的掛載點）。
// exists 讓測試換掉檔案系統
func RepoRootOf(path string, cache map[string]string, exists func(string) (isDir bool, ok bool)) string {
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	for _, skip := range []string{"/Volumes/", "/net/", "/mnt/", "/media/"} {
		if strings.HasPrefix(path, skip) {
			return ""
		}
	}
	dir := path
	if isDir, ok := exists(path); !ok || !isDir {
		dir = filepath.Dir(path)
	}
	var visited []string
	for dir != "" && dir != "/" && dir != "." {
		if known, ok := cache[dir]; ok {
			for _, v := range visited {
				cache[v] = known
			}
			return known
		}
		visited = append(visited, dir)
		if _, ok := exists(filepath.Join(dir, ".git")); ok {
			for _, v := range visited {
				cache[v] = dir
			}
			return dir
		}
		dir = filepath.Dir(dir)
	}
	for _, v := range visited {
		cache[v] = ""
	}
	return ""
}
