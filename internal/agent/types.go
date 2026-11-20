package agent

// 網頁 ↔ 宿主訊息的資料形狀（docs/web-protocol.md 第三節）。欄位名稱照協定（camelCase）；
// 選用的欄位以 omitempty 省略（網頁把沒有欄位與 null 視為相同）。

type Option struct {
	Value       string   `json:"value"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Resolved    *string  `json:"resolved,omitempty"`
	Efforts     []string `json:"efforts,omitempty"`
	Effort      *string  `json:"effort,omitempty"`
}

type Command struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ArgumentHint string `json:"argumentHint"`
}

type Usage struct {
	FetchedAt *int64        `json:"fetchedAt"`
	Stale     bool          `json:"stale"`
	Source    string        `json:"source"`
	Error     *string       `json:"error"`
	Windows   []UsageWindow `json:"windows"`
	Credits   *string       `json:"credits"`
	Note      *string       `json:"note"`
}

type UsageWindow struct {
	Key      string       `json:"key"`
	Label    string       `json:"label"`
	Percent  float64      `json:"percent"`
	ResetsAt *int64       `json:"resetsAt"`
	Severity string       `json:"severity"`
	History  [][2]float64 `json:"history"`
}

type Sessions struct {
	Open       int `json:"open"`
	Running    int `json:"running"`
	Waiting    int `json:"waiting"`
	Background int `json:"background"`
	Programs   int `json:"programs"`
	Subagents  int `json:"subagents"`
}

type AccountView struct {
	ID                  string   `json:"id"`
	Provider            string   `json:"provider"`
	Label               string   `json:"label"`
	Color               string   `json:"color"`
	IsSystemDefault     bool     `json:"isSystemDefault"`
	ExistingFolder      bool     `json:"existingFolder"`
	ConfigDirectory     string   `json:"configDirectory"`
	Email               *string  `json:"email"`
	Plan                *string  `json:"plan"`
	Status              string   `json:"status"`
	StatusText          string   `json:"statusText"`
	SafeMode            bool     `json:"safeMode"`
	ShareCustomizations bool     `json:"shareCustomizations"`
	ResetCreditBusy     bool     `json:"resetCreditBusy"`
	LoginBusy           bool     `json:"loginBusy"`
	DeviceCodeLogin     bool     `json:"deviceCodeLogin"`
	LoginExpiresAt      *int64   `json:"loginExpiresAt"`
	Usage               *Usage   `json:"usage"`
	Sessions            Sessions `json:"sessions"`
}

type ProviderView struct {
	Key                  string `json:"key"`
	Name                 string `json:"name"`
	IsolatedAccounts     bool   `json:"isolatedAccounts"`
	SafeMode             bool   `json:"safeMode"`
	SharedCustomizations bool   `json:"sharedCustomizations"`
	ExistingFolders      []any  `json:"existingFolders"`
}

type CliView struct {
	Installed bool    `json:"installed"`
	Version   *string `json:"version"`
	Path      *string `json:"path"`
	Latest    *string `json:"latest"`
	Busy      bool    `json:"busy"`
	Status    string  `json:"status"`
	Error     *string `json:"error"`
}

type ConversationSummary struct {
	ID               string `json:"id"`
	AccountID        string `json:"accountId"`
	Provider         string `json:"provider"`
	Title            string `json:"title"`
	Group            string `json:"group"`
	UpdatedAt        int64  `json:"updatedAt"`
	WorkingDirectory string `json:"workingDirectory"`
	CustomFolder     bool   `json:"customFolder"`
	Running          bool   `json:"running"`
	Waiting          bool   `json:"waiting"`
	Unread           bool   `json:"unread"`
	Background       bool   `json:"background"`
	ProgramCount     int    `json:"programCount"`
	Titling          bool   `json:"titling"`
}

type Header struct {
	ID               string    `json:"id"`
	AccountID        string    `json:"accountId"`
	Provider         string    `json:"provider"`
	Title            string    `json:"title"`
	Model            string    `json:"model"`
	Effort           string    `json:"effort"`
	DefaultEffort    *string   `json:"defaultEffort"`
	Permission       string    `json:"permission"`
	PermissionModes  []Option  `json:"permissionModes"`
	Models           []Option  `json:"models"`
	Commands         []Command `json:"commands"`
	WorkingDirectory string    `json:"workingDirectory"`
	CustomFolder     bool      `json:"customFolder"`
	RecentFolders    []string  `json:"recentFolders"`
	IsDraft          bool      `json:"isDraft"`
	Running          bool      `json:"running"`
	Steer            bool      `json:"steer"`
	Status           *string   `json:"status"`
	Titling          bool      `json:"titling"`
	StartedAt        *int64    `json:"startedAt"`
	Background       any       `json:"background"`
	Programs         any       `json:"programs"`
	LastTurn         *string   `json:"lastTurn"`
	TurnLive         bool      `json:"turnLive"`
	Context          any       `json:"context"`
	AccountMissing   bool      `json:"accountMissing"`
	AccountDelegate  bool      `json:"accountDelegation"`
	PendingAccount   any       `json:"pendingAccount"`
	Restart          any       `json:"restart"`
	LastPrompt       *Prompt   `json:"lastPrompt"`
}

type Prompt struct {
	ID          string `json:"id"`
	Seq         int64  `json:"seq"`
	Time        int64  `json:"time"`
	Text        string `json:"text"`
	Attachments int    `json:"attachments"`
}

type DiffLine struct {
	T string `json:"t"`
	S string `json:"s"`
}

type DiffStats struct {
	Add int `json:"add"`
	Del int `json:"del"`
}

type QuestionOption struct {
	Label       string  `json:"label"`
	Description string  `json:"description"`
	Preview     *string `json:"preview"`
}

type Question struct {
	ID         string           `json:"id"`
	Header     string           `json:"header"`
	Text       string           `json:"text"`
	Multi      bool             `json:"multi"`
	AllowOther bool             `json:"allowOther"`
	Secret     bool             `json:"secret"`
	Options    []QuestionOption `json:"options"`
}

type PlanStep struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

type Attachment struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Size     string  `json:"size"`
	Path     string  `json:"path"`
	ThumbURL *string `json:"thumbUrl"`
	URL      *string `json:"url"`
}

// Item 是對話裡的一筆（docs/web-protocol.md 的 Item）。存檔也用同一個形狀（items.json）。
type Item struct {
	ID                 string              `json:"id"`
	Kind               string              `json:"kind"`
	Status             string              `json:"status"`
	Time               int64               `json:"time"`
	Seq                int64               `json:"seq"`
	HTML               string              `json:"html"`
	NoticeKind         string              `json:"noticeKind,omitempty"`
	ToolKind           string              `json:"toolKind,omitempty"`
	ToolName           string              `json:"toolName,omitempty"`
	Title              string              `json:"title,omitempty"`
	Detail             string              `json:"detail,omitempty"`
	Output             string              `json:"output,omitempty"`
	Diff               []DiffLine          `json:"diff,omitempty"`
	DiffStats          *DiffStats          `json:"diffStats,omitempty"`
	FilePath           string              `json:"filePath,omitempty"`
	Decision           string              `json:"decision,omitempty"`
	CanAllowForSession bool                `json:"canAllowForSession,omitempty"`
	AllowSessionLabel  string              `json:"allowSessionLabel,omitempty"`
	DetailHTML         string              `json:"detailHtml,omitempty"`
	Questions          []Question          `json:"questions,omitempty"`
	Answers            map[string][]string `json:"answers,omitempty"`
	Plan               []PlanStep          `json:"plan,omitempty"`
	Attachments        []Attachment        `json:"attachments,omitempty"`

	// 以下只在宿主（不送給網頁）
	Text      string `json:"text,omitempty"`
	ToolUseID string `json:"toolUseId,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

type QueuedMessage struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Composer struct {
	Attachments []Attachment    `json:"attachments"`
	Busy        bool            `json:"busy"`
	BusyText    *string         `json:"busyText"`
	Queue       []QueuedMessage `json:"queue"`
	Steering    *string         `json:"steering"`
}
