package agent

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// 對話內的檔案瀏覽與 @ 檔案提示的規則（C# 的 Files/FileBrowser.cs、FileBrowser.Mac.cs 與 Protocol 的 AgentAttachmentRules 的一部分）：
// 列資料夾（資料夾在前、自然排序）、在工作資料夾建檔案索引（快取 60 秒、略過 .git／node_modules 等，bin／packages 只在 .NET 專案旁略過），
// 以子序列模糊比對排序。只認本機的完整路徑（「/」開頭；「//」開頭的不算）。

const (
	fileMaxEntries      = 3000
	fileMaxIndexedFiles = 20_000
	fileMaxDepth        = 12
	fileMaxResults      = 30
	fileIndexLifetime   = 60 * time.Second
	fileMaxCachedIndex  = 8
)

func extSet(list ...string) map[string]bool {
	m := map[string]bool{}
	for _, e := range list {
		m[e] = true
	}
	return m
}

var (
	imageExts            = extSet(".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff")
	videoExts            = extSet(".mp4", ".mov", ".m4v", ".avi", ".mkv", ".webm", ".wmv", ".flv", ".3gp", ".mpg", ".mpeg")
	playableVideoExts    = extSet(".mp4", ".m4v", ".webm", ".mov")
	audioExts            = extSet(".mp3", ".wav", ".m4a", ".aac", ".flac", ".ogg", ".opus", ".wma", ".weba")
	displayableImageExts = extSet(".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".avif") // 刻意不含 SVG：SVG 可帶腳本
	codeExts             = extSet(".cs", ".csproj", ".sln", ".slnx", ".props", ".targets", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".py", ".java", ".kt",
		".go", ".rs", ".c", ".h", ".cpp", ".hpp", ".rb", ".php", ".swift", ".sql", ".sh", ".ps1", ".psm1", ".bat", ".cmd",
		".html", ".htm", ".css", ".scss", ".less", ".vue", ".svelte", ".xaml", ".razor", ".cshtml", ".lua", ".dart", ".r")
	documentExts = extSet(".pdf", ".txt", ".md", ".csv", ".tsv", ".json", ".xml", ".log", ".yaml", ".yml", ".toml", ".ini",
		".doc", ".docx", ".xls", ".xlsx", ".xlsm", ".ppt", ".pptx", ".odt", ".ods", ".odp", ".rtf")
	archiveExts = extSet(".zip", ".7z", ".rar", ".tar", ".gz", ".tgz", ".bz2", ".xz")
	// 點連結時可以直接用預設程式開的（白名單）；其他（執行檔、腳本、HTML、可帶巨集的 Office 格式…）只在檔案管理員中選取
	safeToOpenExts = extSet(".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff", ".ico", ".avif", ".heic",
		".mp4", ".mov", ".m4v", ".avi", ".mkv", ".webm", ".wmv", ".mp3", ".wav", ".m4a", ".aac", ".flac", ".ogg",
		".pdf", ".txt", ".md", ".csv", ".tsv", ".json", ".xml", ".log", ".yaml", ".yml", ".toml",
		".docx", ".xlsx", ".pptx", ".odt", ".ods", ".odp",
		".zip", ".7z", ".rar")
	// 建索引時一律略過的資料夾（建置輸出、套件、版控）；比對不分大小寫
	ignoredDirs = extSet(".git", ".hg", ".svn", "node_modules", "obj", ".vs", ".idea", "__pycache__", ".venv", "venv",
		".next", ".nuxt", ".cache", "testresults", ".gradle", "target", ".pytest_cache", ".mypy_cache")
	// 只在 .NET 專案旁才略過：bin 是 MSBuild 的輸出、packages 是舊版 NuGet 的套件資料夾；其他專案的同名資料夾是原始碼
	dotNetOutputDirs  = extSet("bin", "packages")
	dotNetProjectExts = extSet(".sln", ".slnx", ".csproj", ".fsproj", ".vbproj")
)

func lowerExt(name string) string { return strings.ToLower(path.Ext(name)) }

// classifyForBrowser：檔案瀏覽器的種類 image／video／audio／code／doc／archive／file
func classifyForBrowser(name string) string {
	ext := lowerExt(name)
	switch {
	case imageExts[ext] || displayableImageExts[ext] || ext == ".svg" || ext == ".heic":
		return "image"
	case videoExts[ext]:
		return "video"
	case audioExts[ext]:
		return "audio"
	case codeExts[ext]:
		return "code"
	case documentExts[ext]:
		return "doc"
	case archiveExts[ext]:
		return "archive"
	}
	return "file"
}

func isDisplayableMedia(p string) bool {
	ext := lowerExt(p)
	return displayableImageExts[ext] || playableVideoExts[ext]
}

func isSafeToOpen(p string) bool { return safeToOpenExts[lowerExt(p)] }

// isLocalPath：這台的完整路徑（「/」開頭；「//」開頭的不算，那在 Windows 上是 UNC 的另一種寫法）
func isLocalPath(p string) bool {
	return p != "" && p[0] == '/' && (len(p) == 1 || p[1] != '/')
}

func isIgnoredDir(name string, parentIsDotNet bool) bool {
	n := strings.ToLower(name)
	return ignoredDirs[n] || (parentIsDotNet && dotNetOutputDirs[n])
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

// naturalCompare：自然排序（數字依數值比：img2 在 img10 前面），不分大小寫。連續的數字當成一個數（前面的 0 不影響大小；
// 數值相同時位數少的在前），其他字元逐字比（同 C# 的 FileBrowser.Mac.cs）
func naturalCompare(a, b string) int {
	x, y := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(x) && j < len(y) {
		if isASCIIDigit(x[i]) && isASCIIDigit(y[j]) {
			si, sj := i, j
			for i < len(x) && isASCIIDigit(x[i]) {
				i++
			}
			for j < len(y) && isASCIIDigit(y[j]) {
				j++
			}
			da := strings.TrimLeft(string(x[si:i]), "0")
			db := strings.TrimLeft(string(y[sj:j]), "0")
			if len(da) != len(db) {
				if len(da) < len(db) {
					return -1
				}
				return 1
			}
			if c := strings.Compare(da, db); c != 0 {
				return c
			}
			// 數值相同：前面補的 0 比較少的在前（「1」在「01」前）
			if i-si != j-sj {
				if i-si < j-sj {
					return -1
				}
				return 1
			}
			continue
		}
		ca, cb := unicode.ToUpper(x[i]), unicode.ToUpper(y[j])
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	ra, rb := len(x)-i, len(y)-j
	switch {
	case ra < rb:
		return -1
	case ra > rb:
		return 1
	}
	return 0
}

// FileEntry：檔案瀏覽器的一個項目（web-protocol 的 FileEntry）
type FileEntry struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Kind     string  `json:"kind"`
	Size     *int64  `json:"size"`
	Modified int64   `json:"modified"`
	ThumbURL *string `json:"thumbUrl"`
	URL      *string `json:"url"`
	Hidden   bool    `json:"hidden"`
}

// listDir 列出資料夾內容：先資料夾再檔案，滿了才截斷（混著截斷的話排在後面的子資料夾整個不見）；資料夾在前、自然排序
func listDir(dir string) ([]FileEntry, bool, error) {
	if !isLocalPath(dir) {
		return nil, false, errString("只能瀏覽這台電腦磁碟上的資料夾")
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return nil, false, errString("找不到資料夾：" + dir)
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		if os.IsPermission(err) {
			return nil, false, errString("沒有權限讀取這個資料夾：" + dir)
		}
		return nil, false, err
	}
	var dirs, files []FileEntry
	for _, de := range des {
		full := filepath.Join(dir, de.Name())
		info, err := de.Info()
		if err != nil {
			continue
		}
		isDir := de.IsDir()
		// 符號連結照它指到的東西分類（資料夾的連結可以點進去）
		if de.Type()&os.ModeSymlink != 0 {
			if ti, err := os.Stat(full); err == nil {
				isDir = ti.IsDir()
				info = ti
			}
		}
		e := FileEntry{Name: de.Name(), Path: full, Modified: info.ModTime().UnixMilli(), Hidden: strings.HasPrefix(de.Name(), ".")}
		if isDir {
			e.Kind = "dir"
			dirs = append(dirs, e)
		} else {
			e.Kind = classifyForBrowser(de.Name())
			size := info.Size()
			e.Size = &size
			files = append(files, e)
		}
	}
	all := append(dirs, files...)
	truncated := false
	if len(all) > fileMaxEntries {
		all, truncated = all[:fileMaxEntries], true
	}
	sort.SliceStable(all, func(i, j int) bool {
		di, dj := all[i].Kind == "dir", all[j].Kind == "dir"
		if di != dj {
			return di
		}
		return naturalCompare(all[i].Name, all[j].Name) < 0
	})
	return all, truncated, nil
}

type errString string

func (e errString) Error() string { return string(e) }

// parentDir：上一層資料夾；根目錄回空字串
func parentDir(dir string) string {
	c := filepath.Clean(dir)
	if c == "/" {
		return ""
	}
	return filepath.Dir(c)
}

// buildFileIndex：廣度優先列出檔案（相對路徑，以「/」分隔），略過忽略清單；最多 2 萬筆、12 層
func buildFileIndex(root string) []string {
	type node struct {
		path  string
		depth int
	}
	result := []string{}
	queue := []node{{root, 0}}
	for len(queue) > 0 && len(result) < fileMaxIndexedFiles {
		n := queue[0]
		queue = queue[1:]
		des, err := os.ReadDir(n.path)
		if err != nil {
			continue
		}
		dotNet := false
		var subs []string
		for _, de := range des {
			if de.IsDir() {
				subs = append(subs, de.Name())
				continue
			}
			// 符號連結不跟（可能繞回上層、指到很大的資料夾）；連到檔案的照列
			if de.Type()&os.ModeSymlink != 0 {
				if ti, err := os.Stat(filepath.Join(n.path, de.Name())); err != nil || ti.IsDir() {
					continue
				}
			}
			if !de.Type().IsRegular() && de.Type()&os.ModeSymlink == 0 {
				continue
			}
			dotNet = dotNet || dotNetProjectExts[lowerExt(de.Name())]
			rel, _ := filepath.Rel(root, filepath.Join(n.path, de.Name()))
			result = append(result, filepath.ToSlash(rel))
			if len(result) >= fileMaxIndexedFiles {
				break
			}
		}
		if n.depth >= fileMaxDepth {
			continue
		}
		for _, s := range subs {
			if !isIgnoredDir(s, dotNet) {
				queue = append(queue, node{filepath.Join(n.path, s), n.depth + 1})
			}
		}
	}
	return result
}

// FileSearchResult：@ 檔案提示的一筆
type FileSearchResult struct {
	RelativePath string `json:"relativePath"`
	FullPath     string `json:"fullPath"`
	Kind         string `json:"kind"`
	score        int
}

// searchFiles：模糊搜尋排序（純函式）：檔名命中優先、連續命中與單字開頭加分、路徑短的優先
func searchFiles(rels []string, root, query string, max int) []FileSearchResult {
	// C# 版把「/」換成「\」（Windows 的分隔字元）；這裡的相對路徑一律以「/」分隔，反過來把「\」換成「/」
	q := strings.ReplaceAll(strings.TrimSpace(query), "\\", "/")
	out := []FileSearchResult{}
	for _, rel := range rels {
		var score int
		if q == "" {
			score = 1000 - min(999, len([]rune(rel)))
		} else {
			score = fileScore(q, rel)
			if score <= 0 {
				continue
			}
		}
		out = append(out, FileSearchResult{RelativePath: rel, FullPath: filepath.Join(root, filepath.FromSlash(rel)), Kind: classifyForBrowser(rel), score: score})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.score != b.score {
			return a.score > b.score
		}
		la, lb := len([]rune(a.RelativePath)), len([]rune(b.RelativePath))
		if la != lb {
			return la < lb
		}
		return strings.ToLower(a.RelativePath) < strings.ToLower(b.RelativePath)
	})
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// indexOfFold：不分大小寫的 IndexOf，回字元（rune）位置；找不到回 -1
func indexOfFold(s, sub []rune) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for k := range sub {
			if unicode.ToLower(s[i+k]) != unicode.ToLower(sub[k]) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// fileScore：子序列比對分數；不命中回 0（同 C# 的 FileBrowser.Score，長度以字元計）
func fileScore(query, p string) int {
	q := []rune(query)
	pr := []rune(p)
	name := []rune(path.Base(p))
	plen := len(pr)
	if strings.EqualFold(string(name), query) {
		return 10_000 - plen
	}
	if len(name) >= len(q) && strings.EqualFold(string(name[:len(q)]), query) {
		return 8_000 - plen
	}
	if i := indexOfFold(name, q); i >= 0 {
		return 6_000 - i*10 - plen
	}
	if i := indexOfFold(pr, q); i >= 0 {
		return 4_000 - i - plen
	}
	// 子序列：每個字元依序出現；連續命中、緊接在分隔字元後面加分
	score, qi, streak := 0, 0, 0
	for i := 0; i < len(pr) && qi < len(q); i++ {
		if unicode.ToLower(pr[i]) != unicode.ToLower(q[qi]) {
			streak = 0
			continue
		}
		score += 10 + streak*5
		if i == 0 || strings.ContainsRune("\\/_-. ", pr[i-1]) {
			score += 15
		}
		streak++
		qi++
	}
	if qi == len(q) {
		return max(1, score+1_000-plen)
	}
	return 0
}

// fileIndexCache：各工作資料夾的索引（過期就在背景重建，這次先用舊的；最多快取 8 個）
type fileIndexCache struct {
	mu      sync.Mutex
	entries map[string]*fileIndexEntry
	now     func() time.Time
}

type fileIndexEntry struct {
	builtAt  time.Time
	files    []string
	building bool
}

var fileIndexes = &fileIndexCache{entries: map[string]*fileIndexEntry{}, now: time.Now}

func (fc *fileIndexCache) get(root string) []string {
	fc.mu.Lock()
	e := fc.entries[root]
	if e != nil {
		if fc.now().Sub(e.builtAt) > fileIndexLifetime && !e.building {
			e.building = true
			go func() {
				files := buildFileIndex(root)
				fc.mu.Lock()
				fc.entries[root] = &fileIndexEntry{builtAt: fc.now(), files: files}
				fc.trim(root)
				fc.mu.Unlock()
			}()
		}
		if len(e.files) > 0 || !e.building {
			files := e.files
			fc.mu.Unlock()
			return files
		}
	}
	fc.mu.Unlock()
	files := buildFileIndex(root)
	fc.mu.Lock()
	fc.entries[root] = &fileIndexEntry{builtAt: fc.now(), files: files}
	fc.trim(root)
	fc.mu.Unlock()
	return files
}

// trim：超過上限時移除最久沒重建的（使用中的每分鐘重建一次，最久沒重建的就是最久沒用的）；呼叫時持有 mu
func (fc *fileIndexCache) trim(keep string) {
	for len(fc.entries) > fileMaxCachedIndex {
		oldest := ""
		var at time.Time
		for k, e := range fc.entries {
			if e.building || k == keep {
				continue
			}
			if oldest == "" || e.builtAt.Before(at) {
				oldest, at = k, e.builtAt
			}
		}
		if oldest == "" {
			return
		}
		delete(fc.entries, oldest)
	}
}
